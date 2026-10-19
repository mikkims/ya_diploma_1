package database

import (
	"context"
	"testing"
)

func TestOpenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if db, err := Open(ctx, "host=localhost dbname=synthetic connect_timeout=1"); err == nil || db != nil {
		if db != nil {
			if closeErr := db.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatal("canceled open succeeded")
	}
}
