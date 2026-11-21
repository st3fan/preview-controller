package dashboard

import "testing"

func TestLinuxHostSnapshot(t *testing.T) {
	sample := newHostSampler(t.TempDir())
	s := sample()
	if s.Memory == nil || s.Load == nil || s.Disk == nil {
		t.Fatalf("missing live readings: %+v", s)
	}
	if sample() != s {
		t.Fatal("successive requests did not share a sample")
	}
	if diskUsage("/proc/self/nonexistent-workspace") != nil {
		t.Fatal("missing workspace reported capacity")
	}
	t.Logf("host RAM %s GiB, CPUs %d, workspace %s GiB", s.Memory.Total, s.Load.CPUs, s.Disk.Total)
}
