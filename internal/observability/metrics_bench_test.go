package observability

import (
	"io"
	"testing"
)

func BenchmarkRequestMetric(b *testing.B) {
	for _, name := range []string{"search", "/private/unknown/argument"} {
		b.Run(name, func(b *testing.B) {
			var registry Registry
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					registry.Begin("tool", name)("success")
				}
			})
		})
	}
}

func BenchmarkMetricExposition(b *testing.B) {
	var registry Registry
	for _, name := range []string{"search", "read", "explore", "edit", "change"} {
		registry.Begin("tool", name)("success")
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		registry.Write(NewExposition(io.Discard))
	}
}

func BenchmarkCachedProcessSnapshot(b *testing.B) {
	// A fresh synthetic snapshot exercises only the cached nonblocking path.
	// Restore the shared sampler after the benchmark.
	previous := processCache.Load()
	sample := &ProcessSnapshot{SampleUnix: 1<<62 - 1}
	processCache.Store(sample)
	defer processCache.Store(previous)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Process()
	}
}
