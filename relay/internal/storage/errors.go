package storage

import (
	"errors"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// IsNotFound reports whether err originates from a S3 NoSuchKey (404) response.
// Used to distinguish permanent "object deleted" failures from transient I/O errors.
func IsNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	return errors.As(err, &nsk)
}
