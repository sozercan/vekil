package proxy

import "testing"

func BenchmarkRuntimeSnapshotLoad(b *testing.B) {
	h := &ProxyHandler{}
	h.publishRuntime(&runtimeSnapshot{generation: 1, revision: "cfg_bench", providers: &providerSetup{}, policy: &policyBinding{}, caches: newRuntimeCaches()})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if h.currentRuntime() == nil {
			b.Fatal("runtime snapshot is nil")
		}
	}
}
