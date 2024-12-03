package loadbalancer

import (
	"context"
	"testing"
)

func BenchmarkEndpointGroup(b *testing.B) {
	e := newEndpointGroup()
	e.reconcileEndpoints(map[string]endpoint{"pod1": {address: "10.0.0.1:8000"}})
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, f, err := e.getBestAddr(context.Background(), AddressRequest{}, false)
			if err != nil {
				b.Fatal(err)
			}
			f()
		}
	})
}
