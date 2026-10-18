//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

func namespaceAdapter(t *testing.T, fixture *s3fixture.Fixture, bucket string) *objectstore.S3 {
	t.Helper()
	adapter, err := objectstore.NewS3(context.Background(), fixture.Client(), fixture.Client(), bucket, "")
	requireReceiptNoError(t, err)
	return adapter
}

func namespacePackOptions(t *testing.T) PackOptions {
	t.Helper()
	return PackOptions{
		ScratchDirectory: t.TempDir(),
		MaxPackBytes:     4 << 20,
		MaxStagingBytes:  16 << 20,
		Lease:            10 * time.Second,
	}
}

func TestObjectNamespaceRejectsRestartDriftBeforeReconciliation(t *testing.T) {
	f, p, original, _ := packing(t)
	receipt := accept(t, f.s, newID(t), f.envelope(t), "old namespace bytes")
	job, err := p.Pack(context.Background(), f.org, []ID{receipt.ID})
	requireReceiptNoError(t, err)
	key := objectKey(t, f, job)
	err = p.Close()
	requireReceiptNoError(t, err)
	other := s3fixture.New(s3fixture.Config{})
	defer other.Close()
	for _, adapter := range []*objectstore.S3{
		namespaceAdapter(t, other, "fry-test"),
		namespaceAdapter(t, original, "different-bucket"),
	} {
		opts := namespacePackOptions(t)
		rejected, err := NewPacker(context.Background(), f.s, adapter, opts)
		requireReceiptCondition(
			t,
			errors.Is(err, ErrObjectNamespace) && rejected == nil,
			"namespace drift accepted",
			rejected,
			err,
		)
		requireReceiptCondition(t, f.s.cold.Load() == nil, "rejected namespace attached to receipt store")
	}
	oldObject, ok := original.Object(key)
	requireReceiptCondition(t, ok && len(oldObject.Body) != 0, "old namespace object changed")
	// Reopening independent clients for the same address retains cleanup authority.
	same, err := NewPacker(
		context.Background(),
		f.s,
		namespaceAdapter(t, original, "fry-test"),
		namespacePackOptions(t),
	)
	requireReceiptNoError(t, err)
	defer func() {
		if err := same.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = same.Retire(context.Background(), f.org, job); !errors.Is(err, ErrPackInUse) {
		t.Fatal(err)
	}
}

func TestObjectNamespaceRejectsUnboundHistoricalPacks(t *testing.T) {
	f := setup(t)
	id := newID(t)
	if _, err := f.pool.Exec(
		context.Background(),
		`INSERT INTO fry_pack(id,organization,object_key,state,lease_until,staging_closed) `+
			`VALUES($1,$2,$3,'retired',clock_timestamp(),true)`,
		id.String(),
		f.org.String(),
		"packs/v1/old/unknown",
	); err != nil {
		t.Fatal(err)
	}
	fixture := s3fixture.New(s3fixture.Config{})
	defer fixture.Close()
	if p, err := NewPacker(
		context.Background(),
		f.s,
		namespaceAdapter(t, fixture, "fry-test"),
		namespacePackOptions(t),
	); !errors.Is(err, ErrObjectNamespace) ||
		p != nil {
		t.Fatal("historical keys automatically adopted", p, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_object_namespace WHERE namespace IS NOT NULL`) == 0,
		"failed adoption changed binding",
	)
}

func TestObjectNamespaceConcurrentFirstBindingChoosesExactlyOne(t *testing.T) {
	f := setup(t)
	a, b := s3fixture.New(s3fixture.Config{}), s3fixture.New(s3fixture.Config{})
	defer a.Close()
	defer b.Close()
	adapters := []*objectstore.S3{namespaceAdapter(t, a, "fry-test"), namespaceAdapter(t, b, "fry-test")}
	options := []PackOptions{namespacePackOptions(t), namespacePackOptions(t)}
	start := make(chan struct{})
	type outcome struct {
		packer    *Packer
		err       error
		namespace string
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	for i := range adapters {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			p, err := NewPacker(context.Background(), f.s, adapters[i], options[i])
			results <- outcome{p, err, adapters[i].Namespace()}
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	winners := 0
	var winningNamespace string
	for result := range results {
		if result.err == nil {
			winners++
			winningNamespace = result.namespace
			t.Cleanup(func() {
				if err := result.packer.Close(); err != nil {
					t.Error(err)
				}
			})
		} else if !errors.Is(result.err, ErrObjectNamespace) {
			t.Fatal(result.err)
		}
	}
	requireReceiptConditionf(t, winners == 1, "got %d first bind winners", winners)
	var persisted string
	if err := f.pool.QueryRow(context.Background(), `SELECT namespace FROM fry_object_namespace WHERE singleton`).
		Scan(&persisted); err != nil ||
		persisted != winningNamespace {
		t.Fatal("binding disagrees with winner", persisted, err)
	}
}
