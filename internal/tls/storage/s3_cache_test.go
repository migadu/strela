package storage

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

func TestS3Cache_Get_Success(t *testing.T) {
	mock := NewMockS3Client()
	mock.Storage["test-key"] = []byte("test-certificate-data")

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true },
		Logger:    zap.NewNop(),
	}

	data, err := cache.Get(context.Background(), "test-key")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if string(data) != "test-certificate-data" {
		t.Errorf("Get() returned wrong data: got %q, want %q", string(data), "test-certificate-data")
	}
}

func TestS3Cache_Get_NotFound(t *testing.T) {
	mock := NewMockS3Client()

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true },
		Logger:    zap.NewNop(),
	}

	_, err := cache.Get(context.Background(), "nonexistent-key")
	if err != autocert.ErrCacheMiss {
		t.Errorf("Get() should return ErrCacheMiss for missing key, got: %v", err)
	}
}

func TestS3Cache_Get_S3Error(t *testing.T) {
	mock := NewMockS3Client()
	mock.GetErr = errors.New("S3 connection error")

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true },
		Logger:    zap.NewNop(),
	}

	_, err := cache.Get(context.Background(), "test-key")
	if err == nil {
		t.Error("Get() should return error on S3 failure")
	}
	if err == autocert.ErrCacheMiss {
		t.Error("Get() should not return ErrCacheMiss for S3 errors")
	}
}

func TestS3Cache_Put_AsLeader(t *testing.T) {
	mock := NewMockS3Client()

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true }, // This node is leader
		Logger:    zap.NewNop(),
	}

	testData := []byte("new-certificate-data")
	err := cache.Put(context.Background(), "new-key", testData)
	if err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	// Verify data was written
	if stored, ok := mock.Storage["new-key"]; !ok {
		t.Error("Put() did not write data to S3")
	} else if string(stored) != string(testData) {
		t.Errorf("Put() wrote wrong data: got %q, want %q", string(stored), string(testData))
	}
}

func TestS3Cache_Put_AsNonLeader(t *testing.T) {
	mock := NewMockS3Client()

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return false }, // This node is NOT leader
		Logger:    zap.NewNop(),
	}

	testData := []byte("new-certificate-data")
	err := cache.Put(context.Background(), "new-key", testData)

	// Should succeed silently
	if err != nil {
		t.Errorf("Put() should succeed silently for non-leader, got error: %v", err)
	}

	// Verify data was NOT written
	if _, ok := mock.Storage["new-key"]; ok {
		t.Error("Put() should not write data to S3 when not leader")
	}
}

func TestS3Cache_Put_S3Error(t *testing.T) {
	mock := NewMockS3Client()
	mock.PutErr = errors.New("S3 write error")

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true },
		Logger:    zap.NewNop(),
	}

	err := cache.Put(context.Background(), "test-key", []byte("data"))
	if err == nil {
		t.Error("Put() should return error on S3 failure")
	}
}

func TestS3Cache_Delete_AsLeader(t *testing.T) {
	mock := NewMockS3Client()
	mock.Storage["delete-key"] = []byte("old-data")

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return true },
		Logger:    zap.NewNop(),
	}

	err := cache.Delete(context.Background(), "delete-key")
	if err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	// Verify data was deleted
	if _, ok := mock.Storage["delete-key"]; ok {
		t.Error("Delete() did not remove data from S3")
	}
}

func TestS3Cache_Delete_AsNonLeader(t *testing.T) {
	mock := NewMockS3Client()
	mock.Storage["delete-key"] = []byte("old-data")

	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return false }, // This node is NOT leader
		Logger:    zap.NewNop(),
	}

	err := cache.Delete(context.Background(), "delete-key")

	// Should succeed silently
	if err != nil {
		t.Errorf("Delete() should succeed silently for non-leader, got error: %v", err)
	}

	// Verify data was NOT deleted
	if _, ok := mock.Storage["delete-key"]; !ok {
		t.Error("Delete() should not remove data from S3 when not leader")
	}
}

func TestS3Cache_LeadershipChange(t *testing.T) {
	mock := NewMockS3Client()

	isLeader := true
	cache := &S3Cache{
		S3Client:  mock,
		Bucket:    "test-bucket",
		IsLeaderF: func() bool { return isLeader },
		Logger:    zap.NewNop(),
	}

	// Write as leader
	testData := []byte("cert-data")
	err := cache.Put(context.Background(), "cert-key", testData)
	if err != nil {
		t.Fatalf("Put() as leader failed: %v", err)
	}

	if _, ok := mock.Storage["cert-key"]; !ok {
		t.Error("Leader should have written to S3")
	}

	// Simulate leadership loss
	isLeader = false

	// Try to write as non-leader
	err = cache.Put(context.Background(), "cert-key-2", []byte("new-data"))
	if err != nil {
		t.Errorf("Put() as non-leader should succeed silently: %v", err)
	}

	if _, ok := mock.Storage["cert-key-2"]; ok {
		t.Error("Non-leader should not write to S3")
	}

	// Reading should work regardless of leadership
	data, err := cache.Get(context.Background(), "cert-key")
	if err != nil {
		t.Errorf("Get() should work for non-leader: %v", err)
	}
	if string(data) != string(testData) {
		t.Errorf("Get() returned wrong data: got %q, want %q", string(data), string(testData))
	}
}
