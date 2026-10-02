//go:build darwin

package hostmetrics

import (
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

func TestDarwinNativeSources(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("Apple Silicon native sensors require arm64")
	}
	sampler := New()
	first, err := sampler.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(first)
	t.Log("nativeReceipt", string(data))
	kinds := map[string]bool{}
	for _, v := range first.Temperatures {
		kinds[v.Kind] = true
	}
	if !kinds["cpu"] || !kinds["gpu"] {
		t.Skip("This Mac exposes no CPU/GPU die sensors through unprivileged IOHID")
	}
	if first.GPU == nil || first.GPU.Availability != Available {
		t.Skip("This Mac exposes no IOAccelerator GPU utilization")
	}
	time.Sleep(MinimumInterval)
	second, err := sampler.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(second)
	t.Log("nativeReceipt", string(data))
	if second.Disk == nil || second.Disk.Availability != Available || second.Network == nil || second.Network.Availability != Available {
		t.Skip("This Mac exposes no native disk/network rate baseline")
	}
}
