// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/trillian"
	"github.com/google/trillian/extension"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/merkle/hashers"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/storage/tree"
	"github.com/google/trillian/trees"
	"github.com/google/trillian/types"

	"github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// Used internally by GetLeaves.
	mostRecentRevision = -1
)

var (
	optsMapInit  = trees.NewGetOpts(trees.Admin, trillian.TreeType_MAP)
	optsMapRead  = trees.NewGetOpts(trees.Query, trillian.TreeType_MAP)
	optsMapWrite = trees.NewGetOpts(trees.UpdateMap, trillian.TreeType_MAP)
)

// TODO(codingllama): There is no access control in the server yet and clients could easily modify
// any tree.

// TrillianMapServerOptions allows various options to be provided when creating
// a new TrillianMapServer.
type TrillianMapServerOptions struct {
	// UseSingleTransaction specifies whether updates to a map should be
	// attempted within a single transaction.
	UseSingleTransaction bool

	// UseLargePreload enables the performance workaround applied when
	// UseSingleTransaction is set.
	UseLargePreload bool
}

// TrillianMapServer implements the RPC API defined in the proto
type TrillianMapServer struct {
	trillian.UnimplementedTrillianMapServer
	registry extension.Registry
	opts     TrillianMapServerOptions

	setLeafCounter monitoring.Counter
	getLeafCounter monitoring.Counter
}

// NewTrillianMapServer creates a new RPC server backed by registry
func NewTrillianMapServer(registry extension.Registry, opts TrillianMapServerOptions) *TrillianMapServer {
	if opts.UseSingleTransaction {
		glog.Warning("Using experimental single-transaction mode for map server.")
	}
	mf := registry.MetricFactory
	if mf == nil {
		mf = monitoring.InertMetricFactory{}
	}

	return &TrillianMapServer{
		registry: registry,
		opts:     opts,
		setLeafCounter: mf.NewCounter(
			"set_leaves",
			"Number of map leaves requested to be set",
			"map_id",
		),
		getLeafCounter: mf.NewCounter(
			"get_leaves",
			"Number of map leaves request to be read",
			"map_id",
		),
	}
}

// IsHealthy returns nil if the server is healthy, error otherwise.
func (t *TrillianMapServer) IsHealthy() error {
	ctx, spanEnd := spanFor(context.Background(), "IsHealthy")
	defer spanEnd()
	return t.registry.MapStorage.CheckDatabaseAccessible(ctx)
}

// GetLeaves implements the GetLeaves RPC method.  Each requested index will
// return an inclusion proof to the leaf, or nil if the leaf does not exist.
func (t *TrillianMapServer) GetLeaves(ctx context.Context, req *trillian.GetMapLeavesRequest) (*trillian.GetMapLeavesResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetLeaves")
	defer spanEnd()
	return t.getLeavesByRevision(ctx, req.MapId, req.Index, mostRecentRevision)
}

// GetLeaf returns an inclusion proof to the leaf, or nil if the leaf does not exist.
func (t *TrillianMapServer) GetLeaf(ctx context.Context, req *trillian.GetMapLeafRequest) (*trillian.GetMapLeafResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetLeaf")
	defer spanEnd()
	ret, err := t.getLeavesByRevision(ctx, req.MapId, [][]byte{req.Index}, mostRecentRevision)
	if err != nil {
		return nil, err
	}
	if got := len(ret.MapLeafInclusion); got != 1 {
		return nil, status.Errorf(codes.Internal, "Requested 1 leaf, got %v leaves", got)
	}
	return &trillian.GetMapLeafResponse{
		MapRoot:          ret.MapRoot,
		MapLeafInclusion: ret.MapLeafInclusion[0],
	}, nil
}

// GetLeafByRevision returns an inclusion proof to the leaf, or nil if the leaf does not exist.
func (t *TrillianMapServer) GetLeafByRevision(ctx context.Context, req *trillian.GetMapLeafByRevisionRequest) (*trillian.GetMapLeafResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetLeafByRevision")
	defer spanEnd()
	ret, err := t.getLeavesByRevision(ctx, req.MapId, [][]byte{req.Index}, req.Revision)
	if err != nil {
		return nil, err
	}
	if got := len(ret.MapLeafInclusion); got != 1 {
		return nil, status.Errorf(codes.Internal, "Requested 1 leaf, got %v leaves", got)
	}
	return &trillian.GetMapLeafResponse{
		MapRoot:          ret.MapRoot,
		MapLeafInclusion: ret.MapLeafInclusion[0],
	}, nil
}

// GetLeavesByRevision implements the GetLeavesByRevision RPC method.
func (t *TrillianMapServer) GetLeavesByRevision(ctx context.Context, req *trillian.GetMapLeavesByRevisionRequest) (*trillian.GetMapLeavesResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetLeavesByRevision")
	defer spanEnd()
	if req.Revision < 0 {
		return nil, fmt.Errorf("map revision %d must be >= 0", req.Revision)
	}
	return t.getLeavesByRevision(ctx, req.MapId, req.Index, req.Revision)
}

// GetLeavesByRevisionNoProof implements the GetLeavesByRevision RPC method.
func (t *TrillianMapServer) GetLeavesByRevisionNoProof(ctx context.Context, req *trillian.GetMapLeavesByRevisionRequest) (*trillian.MapLeaves, error) {
	if req.Revision < 0 {
		return nil, fmt.Errorf("map revision %d must be >= 0", req.Revision)
	}
	tree, hasher, err := t.getTreeAndHasher(ctx, req.MapId, optsMapRead)
	if err != nil {
		return nil, fmt.Errorf("could not get map %v: %v", req.MapId, err)
	}
	if err := validateIndices(hasher.Size(), len(req.Index), func(i int) []byte { return req.Index[i] }); err != nil {
		return nil, err
	}

	tx, err := t.snapshotForTree(ctx, tree, "GetLeavesByRevisionNoProof")
	if err != nil {
		return nil, fmt.Errorf("could not create database snapshot: %v", err)
	}
	defer t.closeAndLog(ctx, tree.TreeId, tx, "GetLeavesByRevisionNoProof")

	leaves, err := tx.Get(ctx, req.Revision, req.Index)
	if err != nil {
		return nil, err
	}

	// Remove LeafHash because SetLeaves does not supply it.
	for _, l := range leaves {
		l.LeafHash = nil
	}

	return &trillian.MapLeaves{Leaves: leaves}, nil
}

func (t *TrillianMapServer) getLeavesByRevision(ctx context.Context, mapID int64, indices [][]byte, revision int64) (*trillian.GetMapLeavesResponse, error) {
	tree, hasher, err := t.getTreeAndHasher(ctx, mapID, optsMapRead)
	if err != nil {
		return nil, fmt.Errorf("could not get map %v: %v", mapID, err)
	}

	if err := validateIndices(hasher.Size(), len(indices), func(i int) []byte { return indices[i] }); err != nil {
		return nil, err
	}

	ctx = trees.NewContext(ctx, tree)
	t.getLeafCounter.Add(float64(len(indices)), string(mapID))

	tx, err := t.snapshotForTree(ctx, tree, "GetLeavesByRevision")
	if err != nil {
		return nil, fmt.Errorf("could not create database snapshot: %v", err)
	}
	defer t.closeAndLog(ctx, tree.TreeId, tx, "GetLeavesByRevision")

	var root *trillian.SignedMapRoot
	if revision < 0 {
		// need to know the newest published revision
		r, err := tx.LatestSignedMapRoot(ctx)
		if err != nil {
			return nil, fmt.Errorf("could not fetch the latest SignedMapRoot: %v", err)
		}
		root = r
	} else {
		r, err := tx.GetSignedMapRoot(ctx, revision)
		if err != nil {
			return nil, fmt.Errorf("could not fetch SignedMapRoot %v: %v", revision, err)
		}
		root = r
	}

	var mapRoot types.MapRootV1
	if err := mapRoot.UnmarshalBinary(root.MapRoot); err != nil {
		return nil, err
	}
	revision = int64(mapRoot.Revision)

	// Fetch leaves and their inclusion proofs concurrently:
	wg := &sync.WaitGroup{}

	////////////////////////////////////////////////////
	// Leaves
	leavesByIndex := make(map[string]*trillian.MapLeaf)
	errCh := make(chan error, 2)
	defer close(errCh)
	wg.Add(1)
	go func() {
		defer wg.Done()

		leaves, err := tx.Get(ctx, revision, indices)
		if err != nil {
			errCh <- fmt.Errorf("could not fetch leaves: %v", err)
			return
		}
		for _, l := range leaves {
			leavesByIndex[string(l.Index)] = l
		}
		glog.V(1).Infof("%v: wanted %v leaves, found %v", mapID, len(indices), len(leaves))

		// Add empty leaf values for indices that were not returned.
		for _, index := range indices {
			if _, ok := leavesByIndex[string(index)]; !ok {
				leavesByIndex[string(index)] = &trillian.MapLeaf{Index: index}
			}
		}
	}()
	////////////////////////////////////////////////////

	////////////////////////////////////////////////////
	// Inclusion proofs
	var proofs map[string][][]byte
	wg.Add(1)
	go func() {
		defer wg.Done()

		var err error
		// Fetch inclusion proofs in parallel.
		smtReader := merkle.NewSparseMerkleTreeReader(revision, hasher, tx)
		proofs, err = smtReader.BatchInclusionProof(ctx, revision, indices)
		if err != nil {
			errCh <- fmt.Errorf("could not fetch inclusion proofs: %v", err)
		}
	}()
	////////////////////////////////////////////////////

	wg.Wait()

	select {
	case e := <-errCh:
		return nil, e
	default:
		{
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("could not commit db transaction: %v", err)
	}

	inclusions := make([]*trillian.MapLeafInclusion, len(indices))
	for i, index := range indices {
		inclusions[i] = &trillian.MapLeafInclusion{
			Leaf:      leavesByIndex[string(index)],
			Inclusion: proofs[string(index)],
		}
	}

	return &trillian.GetMapLeavesResponse{
		MapLeafInclusion: inclusions,
		MapRoot:          root,
	}, nil
}

// SetLeaves implements the SetLeaves RPC method.
func (t *TrillianMapServer) SetLeaves(ctx context.Context, req *trillian.SetMapLeavesRequest) (*trillian.SetMapLeavesResponse, error) {
	ctx, spanEnd := spanFor(ctx, "SetLeaves")
	defer spanEnd()

	mapID := req.MapId
	t.setLeafCounter.Add(float64(len(req.Leaves)), string(mapID))

	tree, hasher, err := t.getTreeAndHasher(ctx, mapID, optsMapWrite)
	if err != nil {
		return nil, err
	}
	ctx = trees.NewContext(ctx, tree)

	if err := validateIndices(hasher.Size(), len(req.Leaves), func(i int) []byte { return req.Leaves[i].Index }); err != nil {
		return nil, err
	}

	// Overwrite/set the leaf hashes in the request and create a summary of
	// the leaf indices and new hash values.
	hkv := make([]merkle.HashKeyValue, 0, len(req.Leaves))
	for _, l := range req.Leaves {
		l.LeafHash = hasher.HashLeaf(tree.TreeId, l.Index, l.LeafValue)
		hkv = append(hkv, merkle.HashKeyValue{
			HashedKey:   l.Index,
			HashedValue: l.LeafHash,
		})
	}

	var newRoot *trillian.SignedMapRoot
	err = t.registry.MapStorage.ReadWriteTransaction(ctx, tree, func(ctx context.Context, tx storage.MapTreeTX) error {
		writeRev, err := t.getWriteRevision(ctx, tree, tx, req.Revision)
		if err != nil {
			return err
		}
		glog.V(2).Infof("%v: Writing at revision %v", tree.TreeId, writeRev)

		if err := t.writeLeaves(ctx, tx, req.Leaves); err != nil {
			return err
		}

		newRoot, err = t.updateTree(ctx, tree, hasher, tx, hkv, req.Metadata, writeRev)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &trillian.SetMapLeavesResponse{MapRoot: newRoot}, nil
}

// getWriteRevision returns the revision that this transaction will be written at.
// Only one transaction can be committed for a given revision, thus this transaction
// will compete with any other transactions with the same write revision.
// if assertRev is non-zero then an error will be thrown if assertRev does not match
// the write revision.
func (t *TrillianMapServer) getWriteRevision(ctx context.Context, tree *trillian.Tree, tx storage.MapTreeTX, assertRev int64) (int64, error) {
	writeRev, err := tx.WriteRevision(ctx)
	if err != nil {
		return 0, err
	}
	if assertRev != 0 && writeRev != assertRev {
		return 0, status.Errorf(codes.FailedPrecondition, "can't write to revision %v", assertRev)
	}
	return writeRev, nil
}

// writeLeaves updates the leaf values, but does not calculate nor update the Merkle tree.
func (t *TrillianMapServer) writeLeaves(ctx context.Context, tx storage.MapTreeTX, leaves []*trillian.MapLeaf) error {
	for _, l := range leaves {
		if err := tx.Set(ctx, l.Index, l); err != nil {
			return err
		}
	}
	return nil
}

// updateTree updates the sparse Merkle tree at the specified revision based on the passed-in
// leaf changes, and writes it to the storage. Returns the new signed map root, which is also
// submitted to storage.
func (t *TrillianMapServer) updateTree(ctx context.Context, tree *trillian.Tree, hasher hashers.MapHasher, tx storage.MapTreeTX, hkv []merkle.HashKeyValue, metadata []byte, rev int64) (*trillian.SignedMapRoot, error) {
	// Work around a performance issue when using the map in
	// single-transaction mode by preloading all the nodes we know the
	// sparse Merkle writer is going to need.
	if t.opts.UseSingleTransaction && t.opts.UseLargePreload {
		if err := doPreload(ctx, tx, hasher.BitLen(), hkv); err != nil {
			return nil, err
		}
	}

	smtWriter, err := merkle.NewSparseMerkleTreeWriter(ctx, tree.TreeId, rev, hasher, t.newTXRunner(tree, tx))
	if err != nil {
		return nil, err
	}

	if err = smtWriter.SetLeaves(ctx, hkv); err != nil {
		return nil, err
	}

	rootHash, err := smtWriter.CalculateRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("CalculateRoot(): %v", err)
	}

	newRoot, err := t.makeSignedMapRoot(ctx, tree, time.Now(), rootHash, tree.TreeId, rev, metadata)
	if err != nil {
		return nil, fmt.Errorf("makeSignedMapRoot(): %v", err)
	}

	if err := tx.StoreSignedMapRoot(ctx, newRoot); err != nil {
		return nil, err
	}
	return newRoot, nil
}

func (t *TrillianMapServer) newTXRunner(tree *trillian.Tree, tx storage.MapTreeTX) merkle.TXRunner {
	if t.opts.UseSingleTransaction {
		return &singleTXRunner{tx: tx}
	}
	return &multiTXRunner{tree: tree, mapStorage: t.registry.MapStorage}
}

// singleTXRunner executes all calls to Run with the same underlying transaction.
// If f is large, this may incur a performance penalty.
type singleTXRunner struct {
	tx storage.MapTreeTX
}

// RunTX executes a function in the transaction managed by the singleTXRunner.
func (r *singleTXRunner) RunTX(ctx context.Context, f func(context.Context, storage.MapTreeTX) error) error {
	return f(ctx, r.tx)
}

// multiTXRunner executes each call to Run using its own transaction.
// This allows each invocation of f to proceed independently much faster.
// However, If one transaction fails, the other will still succeed (In some cases this could cause data corruption).
type multiTXRunner struct {
	tree       *trillian.Tree
	mapStorage storage.MapStorage
}

// RunTX executes a function in a new transaction.
func (r *multiTXRunner) RunTX(ctx context.Context, f func(context.Context, storage.MapTreeTX) error) error {
	return r.mapStorage.ReadWriteTransaction(ctx, r.tree, f)
}

// doPreload causes the subtreeCache in tx to become populated with all subtrees
// on the Merkle path for the indices specified in hkv.
// This is a performance workaround for locking issues which occur when the
// sparse Merkle tree code is used with a single transaction (and therefore
// a single subtreeCache too).
func doPreload(ctx context.Context, tx storage.MapTreeTX, treeDepth int, hkv []merkle.HashKeyValue) error {
	ctx, spanEnd := spanFor(ctx, "doPreload")
	defer spanEnd()

	readRev, err := tx.ReadRevision(ctx)
	if err != nil {
		return err
	}

	nids := calcAllSiblingsParallel(ctx, treeDepth, hkv)
	_, err = tx.GetMerkleNodes(ctx, readRev, nids)
	return err
}

func calcAllSiblingsParallel(_ context.Context, treeDepth int, hkv []merkle.HashKeyValue) []tree.NodeID {
	type nodeAndID struct {
		id   string
		node tree.NodeID
	}
	c := make(chan nodeAndID, 2048)
	var wg sync.WaitGroup

	// Kick off producers.
	for _, i := range hkv {
		wg.Add(1)
		go func(k []byte) {
			defer wg.Done()
			nid := tree.NewNodeIDFromHash(k)
			sibs := nid.Siblings()
			for _, sib := range sibs {
				sibID := sib.AsKey()
				sib := sib
				c <- nodeAndID{sibID, sib}
			}
		}(i.HashedKey)
	}

	// monitor for all the producers being complete to close the channel.
	go func() {
		wg.Wait()
		close(c)
	}()

	nidSet := make(map[string]bool)
	nids := make([]tree.NodeID, 0, len(hkv)*treeDepth)
	// consume the produced IDs until the channel is closed.
	for nai := range c {
		if _, ok := nidSet[nai.id]; !ok {
			nidSet[nai.id] = true
			nids = append(nids, nai.node)
		}
	}

	return nids
}

func (t *TrillianMapServer) makeSignedMapRoot(ctx context.Context, tree *trillian.Tree, smrTs time.Time,
	rootHash []byte, mapID, revision int64, meta []byte) (*trillian.SignedMapRoot, error) {
	smr := &types.MapRootV1{
		RootHash:       rootHash,
		TimestampNanos: uint64(smrTs.UnixNano()),
		Revision:       uint64(revision),
		Metadata:       meta,
	}
	signer, err := trees.Signer(ctx, tree)
	if err != nil {
		return nil, fmt.Errorf("trees.Signer(): %v", err)
	}
	root, err := signer.SignMapRoot(smr)
	if err != nil {
		return nil, fmt.Errorf("SignMapRoot(): %v", err)
	}
	return root, nil
}

// GetSignedMapRoot implements the GetSignedMapRoot RPC method.
func (t *TrillianMapServer) GetSignedMapRoot(ctx context.Context, req *trillian.GetSignedMapRootRequest) (*trillian.GetSignedMapRootResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetSignedMapRoot")
	defer spanEnd()
	tree, ctx, err := t.getTreeAndContext(ctx, req.MapId, optsMapRead)
	if err != nil {
		return nil, err
	}
	tx, err := t.snapshotForTree(ctx, tree, "GetSignedMapRoot")
	if err != nil {
		return nil, err
	}
	defer t.closeAndLog(ctx, tree.TreeId, tx, "GetSignedMapRoot")

	r, err := tx.LatestSignedMapRoot(ctx)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		glog.Warningf("%v: Commit failed for GetSignedMapRoot: %v", req.MapId, err)
		return nil, err
	}

	return &trillian.GetSignedMapRootResponse{MapRoot: r}, nil
}

// GetSignedMapRootByRevision implements the GetSignedMapRootByRevision RPC
// method.
func (t *TrillianMapServer) GetSignedMapRootByRevision(ctx context.Context, req *trillian.GetSignedMapRootByRevisionRequest) (*trillian.GetSignedMapRootResponse, error) {
	ctx, spanEnd := spanFor(ctx, "GetSignedMapRootByRevision")
	defer spanEnd()
	if req.Revision < 0 {
		return nil, fmt.Errorf("map revision %d must be >= 0", req.Revision)
	}
	tree, ctx, err := t.getTreeAndContext(ctx, req.MapId, optsMapRead)
	if err != nil {
		return nil, err
	}
	tx, err := t.snapshotForTree(ctx, tree, "GetSignedMapRootByRevision")
	if err != nil {
		return nil, err
	}
	defer t.closeAndLog(ctx, tree.TreeId, tx, "GetSignedMapRootByRevision")

	r, err := tx.GetSignedMapRoot(ctx, req.Revision)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		glog.Warningf("%v: Commit failed for GetSignedMapRootByRevision: %v", req.MapId, err)
		return nil, err
	}

	return &trillian.GetSignedMapRootResponse{MapRoot: r}, nil
}

func (t *TrillianMapServer) getTreeAndHasher(ctx context.Context, treeID int64, opts trees.GetOpts) (*trillian.Tree, hashers.MapHasher, error) {
	tree, err := trees.GetTree(ctx, t.registry.AdminStorage, treeID, opts)
	if err != nil {
		return nil, nil, err
	}
	th, err := hashers.NewMapHasher(tree.HashStrategy)
	if err != nil {
		return nil, nil, err
	}
	return tree, th, nil
}

func (t *TrillianMapServer) getTreeAndContext(ctx context.Context, treeID int64, opts trees.GetOpts) (*trillian.Tree, context.Context, error) {
	tree, err := trees.GetTree(ctx, t.registry.AdminStorage, treeID, opts)
	if err != nil {
		return nil, nil, err
	}
	return tree, trees.NewContext(ctx, tree), nil
}

// InitMap implements the RPC Method of the same name.
func (t *TrillianMapServer) InitMap(ctx context.Context, req *trillian.InitMapRequest) (*trillian.InitMapResponse, error) {
	ctx, spanEnd := spanFor(ctx, "InitMap")
	defer spanEnd()
	mapID := req.MapId
	tree, hasher, err := t.getTreeAndHasher(ctx, mapID, optsMapInit)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "getTreeAndHasher(): %v", err)
	}
	ctx = trees.NewContext(ctx, tree)

	var rev0Root *trillian.SignedMapRoot
	err = t.registry.MapStorage.ReadWriteTransaction(ctx, tree, func(ctx context.Context, tx storage.MapTreeTX) error {
		// Check that the map actually needs initialising
		latestRoot, err := tx.LatestSignedMapRoot(ctx)
		if err != nil && err != storage.ErrTreeNeedsInit {
			return status.Errorf(codes.FailedPrecondition, "LatestSignedMapRoot(): %v", err)
		}
		// Belt and braces check.
		if latestRoot.GetMapRoot() != nil {
			return status.Errorf(codes.AlreadyExists, "map is already initialised")
		}

		rev0Root = nil

		glog.V(2).Infof("%v: Need to init map root revision 0", mapID)
		rootHash := hasher.HashEmpty(mapID, make([]byte, hasher.Size()), hasher.BitLen())
		rev0Root, err = t.makeSignedMapRoot(ctx, tree, time.Now(), rootHash, mapID, 0 /*revision*/, nil /* metadata */)
		if err != nil {
			return fmt.Errorf("makeSignedMapRoot(): %v", err)
		}

		return tx.StoreSignedMapRoot(ctx, rev0Root)
	})
	if err != nil {
		return nil, err
	}

	return &trillian.InitMapResponse{
		Created: rev0Root,
	}, nil
}

func (t *TrillianMapServer) closeAndLog(ctx context.Context, logID int64, tx storage.ReadOnlyMapTreeTX, op string) {
	err := tx.Close()
	if err != nil {
		glog.Warningf("%v: Close failed for %v: %v", logID, op, err)
	}
}

func (t *TrillianMapServer) snapshotForTree(ctx context.Context, tree *trillian.Tree, method string) (storage.ReadOnlyMapTreeTX, error) {
	tx, err := t.registry.MapStorage.SnapshotForTree(ctx, tree)
	if err != nil && tx != nil {
		// Special case to handle ErrTreeNeedsInit, which leaves the TX open.
		// To avoid leaking it make sure it's closed.
		defer t.closeAndLog(ctx, tree.TreeId, tx, method)
	}
	return tx, err
}

// validateIndices confirms that all indices have the given size and there are no duplicates.
// indexSize is the expected size of each index in bytes.
// n is the number of indices to check.
// indices is a function that returns indices from [0 .. n).
func validateIndices(indexSize, n int, indices func(i int) []byte) error {
	// The parameter is named 'index' (here and in the RPC API) because it's the ordinal number
	// of the leaf, but that number is obtained by hashing the key value that corresponds to the
	// leaf.  Leaf "indices" are therefore sparsely scattered in the range [0, 2^hashsize) and
	// are represented as a []byte, and every leaf must have an index that is the same size.
	//
	// We currently police this by requiring that the hash size for the index space be the same
	// as the hash size for the tree itself, although that's not strictly required (e.g. could
	// have SHA-256 for generating leaf indices, but SHA-512 for building the root hash).
	seenIndices := make(map[string]bool)
	for i := 0; i < n; i++ {
		index := indices(i)
		if got, want := len(index), indexSize; got != want {
			return status.Errorf(codes.InvalidArgument, "index at position %d has wrong length: got=%d,want=%d", i, got, want)
		}
		if seenIndices[string(index)] {
			return status.Errorf(codes.InvalidArgument, "duplicate index detected at position %d", i)
		}
		seenIndices[string(index)] = true
	}
	return nil
}
