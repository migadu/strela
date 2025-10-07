// Package storage provides S3-backed certificate storage for autocert with
// leader-based write coordination. This enables multi-node Let's Encrypt
// certificate management without duplicate ACME challenges.
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

// S3API defines the S3 operations required for certificate storage.
// This interface allows for mocking in tests and abstracts the AWS SDK.
type S3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3Cache implements autocert.Cache using an S3 bucket with leader-based write
// coordination. In multi-node clusters, only the leader node performs certificate
// writes (Put/Delete), while all nodes can read certificates (Get). This prevents
// race conditions during Let's Encrypt ACME challenges.
//
// Leadership is determined by calling IsLeaderF, which should return true only
// for the current cluster leader. Non-leader writes are silently ignored.
type S3Cache struct {
	S3Client  S3API
	Bucket    string
	IsLeaderF func() bool
	Logger    *zap.Logger
}

// Get reads certificate data from S3. All nodes (leader and non-leader) can read
// certificates. Returns autocert.ErrCacheMiss if the certificate is not found,
// allowing autocert to trigger new certificate issuance.
//
// Uses a 10-second timeout to prevent indefinite hangs on S3 issues.
func (s *S3Cache) Get(ctx context.Context, key string) ([]byte, error) {
	// Add timeout to prevent indefinite hangs on S3 issues
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := s.S3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			s.Logger.Debug("certificate not found in S3", zap.String("key", key))
			return nil, autocert.ErrCacheMiss
		}
		s.Logger.Error("failed to get certificate from S3", zap.String("key", key), zap.Error(err))
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		s.Logger.Error("failed to read certificate data", zap.String("key", key), zap.Error(err))
		return nil, err
	}

	s.Logger.Info("certificate retrieved from S3", zap.String("key", key), zap.Int("size", len(data)))
	return data, nil
}

// Put writes certificate data to S3, but only if the node is the cluster leader.
// Non-leader nodes silently skip the write and return nil, allowing them to
// participate in certificate retrieval without causing conflicts.
//
// Leadership is checked before and after the write operation to detect race
// conditions where leadership changes during the operation. Uses a 10-second
// timeout to prevent indefinite hangs on S3 issues.
func (s *S3Cache) Put(ctx context.Context, key string, data []byte) error {
	// Check leadership before operation (prevents race condition)
	wasLeader := s.IsLeaderF()
	if !wasLeader {
		s.Logger.Debug("skipping certificate write (not leader)", zap.String("key", key))
		return nil
	}

	// Add timeout to prevent indefinite hangs on S3 issues
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := s.S3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		s.Logger.Error("failed to put certificate to S3", zap.String("key", key), zap.Error(err))
		return err
	}

	// Check if we're still leader after the write (detect race condition)
	if !s.IsLeaderF() && wasLeader {
		s.Logger.Warn("leadership changed during certificate write - write completed but node is no longer leader",
			zap.String("key", key))
	}

	s.Logger.Info("certificate stored in S3", zap.String("key", key), zap.Int("size", len(data)))
	return nil
}

// Delete removes certificate data from S3, but only if the node is the cluster
// leader. Non-leader nodes silently skip the delete and return nil. Leadership
// is checked before and after the operation to detect race conditions.
//
// Uses a 10-second timeout to prevent indefinite hangs on S3 issues.
func (s *S3Cache) Delete(ctx context.Context, key string) error {
	// Check leadership before operation (prevents race condition)
	wasLeader := s.IsLeaderF()
	if !wasLeader {
		s.Logger.Debug("skipping certificate delete (not leader)", zap.String("key", key))
		return nil
	}

	// Add timeout to prevent indefinite hangs on S3 issues
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := s.S3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		s.Logger.Error("failed to delete certificate from S3", zap.String("key", key), zap.Error(err))
		return err
	}

	// Check if we're still leader after the delete (detect race condition)
	if !s.IsLeaderF() && wasLeader {
		s.Logger.Warn("leadership changed during certificate delete - delete completed but node is no longer leader",
			zap.String("key", key))
	}

	s.Logger.Info("certificate deleted from S3", zap.String("key", key))
	return nil
}
