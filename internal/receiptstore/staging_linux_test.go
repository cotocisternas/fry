//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func TestStagingRecoveryRemovesScratchWithoutUnpublishingLivePack(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "unpublished"
		if published {
			name = "published"
		}
		t.Run(name, func(t *testing.T) {
			testStagingRecovery(t, published)
		})
	}
}

func TestPackerScratchHasOneOwner(t *testing.T) {
	f, p, _, _ := packing(t)
	second, err := New(context.Background(), f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	if duplicate, err := NewPacker(context.Background(), second, p.objects, p.opts); !errors.Is(err, hotstore.ErrBusy) {
		if duplicate != nil {
			if err := duplicate.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatal("two scratch owners", err)
	}
}

func testStagingRecovery(t *testing.T, published bool) {
	t.Helper()

	f, p, _, _ := packing(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "private source")
	if !published {
		p.afterUpload = func(ID) error { return io.ErrUnexpectedEOF }
	}
	id, err := p.Pack(ctx, f.org, []ID{r.ID})
	requireReceiptCondition(t, !published || err == nil, err)
	requireReceiptCondition(t, published || errors.Is(err, io.ErrUnexpectedEOF), err)
	// Reconstruct the durable boundary left by a crash before scratch cleanup.
	dir := filepath.Join(p.opts.ScratchDirectory, "fry-pack-"+id.String())
	err = os.Mkdir(dir, 0o700)
	requireReceiptNoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "data.fry"), []byte("private scratch copy"), 0o600)
	requireReceiptNoError(t, err)
	execSQL(t, f.pool, "UPDATE fry_pack SET staging_closed=false WHERE id=$1", id.String())
	if err := p.Close(); err != nil {
		t.Error(err)
	}
	next, err := NewPacker(context.Background(), f.s, p.objects, p.opts)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	})
	{
		_, err = os.Stat(dir)
		requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "scratch copy retained", err)
	}
	state, err := next.Status(ctx, f.org, id)
	requireReceiptNoError(t, err)
	want := "retiring"
	if published {
		want = "published"
	}
	requireReceiptCondition(t, state == want, state, want)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_pack WHERE NOT staging_closed") == 0,
		"staging cleanup not recorded",
	)
}
