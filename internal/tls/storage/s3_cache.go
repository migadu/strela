package storage

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

// S3Cache implements autocert.Cache using an S3 bucket.
// It only allows writes if the current node is the cluster leader.
type S3Cache struct {
	S3Client  *s3.Client
	Bucket    string
	IsLeaderF func() bool
	Logger    *zap.Logger
}

// Get reads a certificate data from S3.
func (s *S3Cache) Get(ctx context.Context, key string) ([]byte, error) {
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

// Put writes a certificate data to S3, only if the node is the leader.
func (s *S3Cache) Put(ctx context.Context, key string, data []byte) error {
	if !s.IsLeaderF() {
		s.Logger.Debug("skipping certificate write (not leader)", zap.String("key", key))
		return nil
	}

	_, err := s.S3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		s.Logger.Error("failed to put certificate to S3", zap.String("key", key), zap.Error(err))
		return err
	}

	s.Logger.Info("certificate stored in S3", zap.String("key", key), zap.Int("size", len(data)))
	return nil
}

// Delete removes a certificate data from S3, only if the node is the leader.
func (s *S3Cache) Delete(ctx context.Context, key string) error {
	if !s.IsLeaderF() {
		s.Logger.Debug("skipping certificate delete (not leader)", zap.String("key", key))
		return nil
	}

	_, err := s.S3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		s.Logger.Error("failed to delete certificate from S3", zap.String("key", key), zap.Error(err))
		return err
	}

	s.Logger.Info("certificate deleted from S3", zap.String("key", key))
	return nil
}
