package persistence

import "testing"

// The TxHandle seal: embedding SealedTxHandle is the ONLY way to satisfy the
// interface — the promotion is the proof a handle is framework-issued.
func TestTxHandleSeal_PromotesThroughEmbedding(t *testing.T) {
	SealedTxHandle{}.txHandle()
	type adapterHandle struct{ SealedTxHandle }
	var h any = adapterHandle{}
	if _, ok := h.(TxHandle); !ok {
		t.Fatal("embedding SealedTxHandle must satisfy TxHandle via promotion")
	}
}
