// This file wires the scheme-aware state-capability policy in eth/arb onto the
// concrete *Ethereum node. It lives in package eth (not eth/arb) so it can touch
// *core.BlockChain and *triedb.Database directly without creating an import cycle
// — eth/arb stays node-agnostic and unit-testable, this adapter binds it.
//
// NODE-00: read-only state lifecycle. hash scheme pins via triedb.Reference;
// path scheme cannot pin (Reference returns "not supported"), so arb.Acquire
// only ever calls Pin on hash scheme and treats path as best-effort. This file
// does not start any service, open any port, or restart anything — it only
// exposes the backend capability surface.

package eth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/eth/arb"
)

// arbStateBackend adapts *Ethereum to arb.Backend. Read-only; holds no state of
// its own beyond the node handle.
type arbStateBackend struct {
	eth *Ethereum
}

// NewArbStateBackend returns the arb.Backend view of this node. Exported for the
// arb service constructor (added in a later NODE task) and for integration tests.
func (s *Ethereum) NewArbStateBackend() arb.Backend {
	return &arbStateBackend{eth: s}
}

// Scheme maps the node's rawdb scheme onto the policy enum.
func (b *arbStateBackend) Scheme() arb.StateScheme {
	if b.eth.blockchain.TrieDB().Scheme() == rawdb.PathScheme {
		return arb.SchemePath
	}
	return arb.SchemeHash
}

// CurrentHeadRoot returns the canonical head state root and its number.
func (b *arbStateBackend) CurrentHeadRoot() (common.Hash, uint64) {
	h := b.eth.blockchain.CurrentBlock()
	if h == nil {
		return common.Hash{}, 0
	}
	return h.Root, h.Number.Uint64()
}

// Pin places a GC reference on root. For path scheme triedb.Reference returns an
// error ("not supported"), which is the correct, honest signal — the policy layer
// only calls Pin for hash scheme, so this stays consistent with the contract.
func (b *arbStateBackend) Pin(root common.Hash) error {
	return b.eth.blockchain.TrieDB().Reference(root, common.Hash{})
}

// Unpin removes a reference. Only meaningful (and only invoked) for hash scheme.
func (b *arbStateBackend) Unpin(root common.Hash) error {
	return b.eth.blockchain.TrieDB().Dereference(root)
}

// Readable probes whether root resolves against the live state right now. This is
// a single-shot probe and may race with concurrent import (which is exactly why
// path handles are best-effort and every read re-checks).
func (b *arbStateBackend) Readable(root common.Hash) bool {
	_, err := b.eth.blockchain.StateAt(root)
	return err == nil
}

// CanServeHistoric reports whether a non-head root can be served from an
// immutable historic source. We probe HistoricState; it succeeds only when the
// state indexer/freezer can back the root, so a true result is a proven reader,
// not a guess.
func (b *arbStateBackend) CanServeHistoric(root common.Hash) bool {
	_, err := b.eth.blockchain.HistoricState(root)
	return err == nil
}
