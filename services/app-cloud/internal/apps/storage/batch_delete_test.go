package storage

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

type countingMeasure struct {
	*fakeMultipart
	measures *atomic.Int64
}

func (c countingMeasure) MeasureVersions(ctx context.Context, b string) (int64, int64, error) {
	c.measures.Add(1)
	return c.fakeMultipart.MeasureVersions(ctx, b)
}

// A batch may hold 1,000 keys. Re-listing every version of every project bucket
// after each key makes the request quadratic, so a large batch outlives the
// console's request and the remaining keys are reported as failed.
func TestBatchDeleteReconcilesUsageOncePerBatch(t *testing.T) {
	m, f, _ := testModule(t)
	var measures atomic.Int64
	m.plane = countingMeasure{multipartFake(f), &measures}
	s := scope()
	b := makeBucket(t, m, s, "private")
	ctx := context.Background()
	keys := []string{}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("batch/f%02d.txt", i)
		if e := f.Put(ctx, b.Name, "files/"+key, strings.NewReader("xy"), 2, "text/plain"); e != nil {
			t.Fatal(e)
		}
		keys = append(keys, key)
	}
	synced := decode[Bucket](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/sync", nil), 200)
	if synced.UsedBytes != 40 || synced.ObjectCount != 20 {
		t.Fatalf("sync: %+v", synced)
	}
	measures.Store(0)
	v := decode[struct {
		Deleted []string `json:"deleted"`
		Failed  []string `json:"failed"`
	}](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/delete-batch", map[string]any{"keys": append(keys, "batch/missing.txt")}), 200)
	if len(v.Deleted) != 20 || len(v.Failed) != 1 || v.Failed[0] != "batch/missing.txt" {
		t.Fatalf("batch result: %+v", v)
	}
	if n := measures.Load(); n != 1 {
		t.Fatalf("batch of %d deletes measured project usage %d times, want once", len(keys), n)
	}
	after := decode[Bucket](t, request(t, m, s, "GET", "/buckets/"+b.ID, nil), 200)
	if after.UsedBytes != 0 || after.ObjectCount != 0 {
		t.Fatalf("usage not reconciled after batch: %+v", after)
	}
	// A single delete still reconciles immediately.
	_ = f.Put(ctx, b.Name, "files/single.txt", strings.NewReader("abc"), 3, "text/plain")
	measures.Store(0)
	decode[map[string]bool](t, request(t, m, s, "DELETE", "/buckets/"+b.ID+"/objects?key=single.txt", nil), 200)
	if measures.Load() != 1 {
		t.Fatal("single delete did not reconcile usage")
	}
}
