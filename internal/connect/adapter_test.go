package connect

import (
	"context"
	"testing"
)

type fakeAdapter struct{}

func (fakeAdapter) Send(context.Context, SendRequest) (SendResponse, error) {
	return SendResponse{}, nil
}

func TestConnectRegistry(t *testing.T) {
	r := NewRegistry()
	if got := r.Adapter(ProviderSatusehat); got != nil {
		t.Fatalf("unknown code before Register: got %v, want nil", got)
	}
	want := fakeAdapter{}
	r.Register(ProviderSatusehat, want)
	if got := r.Adapter(ProviderSatusehat); got != want {
		t.Fatalf("Adapter(%q) = %v, want registered adapter", ProviderSatusehat, got)
	}
}

func TestConnectRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	r := NewRegistry()
	r.Register(ProviderSatusehat, fakeAdapter{})
	r.Register(ProviderSatusehat, fakeAdapter{})
}
