package objstore

import (
	"context"
	"net/http"
)

// CreateBucketForTest issues a signed "PUT /<bucket>" (used by the optional
// test against a real S3 implementation). An already existing bucket is fine.
func (s *S3Store) CreateBucketForTest(ctx context.Context) error {
	req, err := s.newRequest(ctx, http.MethodPut, "", nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, emptySHA256, "create-bucket", s.bucket, http.StatusOK, http.StatusConflict)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
