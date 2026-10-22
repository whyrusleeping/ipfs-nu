package nutest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkCheckpointIO isolates the destination device: sequential 64 KiB
// writes with a durable checkpoint every 16 MiB. No Nu, network, Bao, SQLite,
// goroutines, locks or scheduler. Uses the same opt-in and size as Download.
func BenchmarkCheckpointIO(b *testing.B) {
	size := downloadSize(b)
	b.StopTimer()
	buf := make([]byte, 64<<10)
	for i := 0; i < b.N; i++ {
		path := filepath.Join(b.TempDir(), "data")
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		var syncTime, maxSync time.Duration
		b.StartTimer()
		start := time.Now()
		for off := int64(0); off < size; {
			n, err := f.Write(buf[:min(int64(len(buf)), size-off)])
			if err != nil {
				b.Fatal(err)
			}
			off += int64(n)
			if off%(16<<20) == 0 || off == size {
				at := time.Now()
				err := f.Sync()
				elapsed := time.Since(at)
				if err != nil {
					b.Fatal(err)
				}
				syncTime += elapsed
				maxSync = max(maxSync, elapsed)
			}
		}
		elapsed := time.Since(start)
		b.StopTimer()
		b.ReportMetric(maxSync.Seconds(), "max-sync-sec")
		b.ReportMetric(syncTime.Seconds()/elapsed.Seconds(), "sync-fraction")
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			b.Fatal(err)
		}
	}
}
