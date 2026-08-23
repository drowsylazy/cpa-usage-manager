package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyEndpointCRUD(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "notify.db"), OwnerID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	id, err := st.InsertNotifyEndpoint(ctx, "主通道", []byte("cipher-aes"), true, time.Time{})
	if err != nil || id <= 0 {
		t.Fatalf("insert=%d err=%v", id, err)
	}
	list, err := st.ListNotifyEndpoints(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	if list[0].Label != "主通道" || string(list[0].URLEnc) != "cipher-aes" || !list[0].Enabled {
		t.Fatalf("endpoint=%+v", list[0])
	}
	got, err := st.GetNotifyEndpoint(ctx, id)
	if err != nil || got.ID != id {
		t.Fatalf("get=%+v err=%v", got, err)
	}

	if err := st.UpdateNotifyEndpoint(ctx, id, "备用通道", []byte("cipher-2"), false); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListNotifyEndpoints(ctx)
	if list[0].Label != "备用通道" || string(list[0].URLEnc) != "cipher-2" || list[0].Enabled {
		t.Fatalf("update 后=%+v", list[0])
	}

	ok1 := time.Now().UTC().Add(-time.Hour)
	if err := st.UpdateNotifyEndpointResult(ctx, id, ok1, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetNotifyEndpoint(ctx, id)
	if got.LastOKAt == nil || got.LastError != "" {
		t.Fatalf("成功结果未记录: %+v", got)
	}
	ok2 := time.Now().UTC()
	if err := st.UpdateNotifyEndpointResult(ctx, id, ok2, "connection refused"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetNotifyEndpoint(ctx, id)
	if got.LastError != "connection refused" {
		t.Fatalf("失败原因未记录: %+v", got)
	}
	if got.LastOKAt == nil || !got.LastOKAt.Equal(ok1.Truncate(time.Millisecond)) {
		t.Fatalf("失败不应刷新 last_ok_at: %+v vs %v", got.LastOKAt, ok1)
	}
	if got.LastSentAt == nil || got.LastSentAt.Before(ok2.Add(-time.Second)) {
		t.Fatalf("last_sent_at 应刷新: %+v", got.LastSentAt)
	}

	if err := st.DeleteNotifyEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetNotifyEndpoint(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 NotFound: %v", err)
	}
	if err := st.DeleteNotifyEndpoint(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 NotFound: %v", err)
	}
}
