package store_test

import (
	"context"
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestOpenEmptyDSNUsesMemory(t *testing.T) {
	st, cst, closer, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	if _, ok := st.(*store.Memory); !ok {
		t.Fatalf("store type %T", st)
	}
	if _, ok := cst.(*store.Memory); !ok {
		t.Fatalf("controller store type %T", cst)
	}
}

func TestOpenWithoutSchemaEmptyDSNUsesMemory(t *testing.T) {
	st, cst, closer, err := store.OpenWithoutSchema(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	if _, ok := st.(*store.Memory); !ok {
		t.Fatalf("store type %T", st)
	}
	if _, ok := cst.(*store.Memory); !ok {
		t.Fatalf("controller store type %T", cst)
	}
}
