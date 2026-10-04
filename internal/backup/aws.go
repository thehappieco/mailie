package backup

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// AWS is KMS and S3 through the SDK. It implements KMS, Uploader and
// Downloader.
type AWS struct {
	kms *kms.Client
	s3  *s3.Client
}

// NewAWS builds the clients for region from the SDK's default credential
// chain: on the host, its own identity (an EC2 instance role through IMDSv2,
// say); where a restore runs, the signed-in principal's credentials. No
// credential is ever read from this program's configuration.
func NewAWS(ctx context.Context, region string) (*AWS, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("backup: AWS configuration: %w", err)
	}
	return newAWS(cfg, nil, nil), nil
}

// newAWS builds the clients from cfg, with options a test uses to point them
// at a local server.
func newAWS(cfg aws.Config, kmsOpts []func(*kms.Options), s3Opts []func(*s3.Options)) *AWS {
	return &AWS{kms: kms.NewFromConfig(cfg, kmsOpts...), s3: s3.NewFromConfig(cfg, s3Opts...)}
}

// GenerateDataKey asks KMS for an AES-256 data key.
func (a *AWS) GenerateDataKey(ctx context.Context, keyARN string, encCtx map[string]string) (DataKey, error) {
	out, err := a.kms.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:             aws.String(keyARN),
		KeySpec:           kmstypes.DataKeySpecAes256,
		EncryptionContext: encCtx,
	})
	if err != nil {
		return DataKey{}, err
	}
	return DataKey{KeyARN: aws.ToString(out.KeyId), Plaintext: out.Plaintext, Wrapped: out.CiphertextBlob}, nil
}

// Decrypt asks KMS to unwrap a data key, naming the key it must have been
// wrapped under.
func (a *AWS) Decrypt(ctx context.Context, keyARN string, wrapped []byte, encCtx map[string]string) ([]byte, error) {
	out, err := a.kms.Decrypt(ctx, &kms.DecryptInput{
		KeyId:               aws.String(keyARN),
		CiphertextBlob:      wrapped,
		EncryptionContext:   encCtx,
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecSymmetricDefault,
	})
	if err != nil {
		return nil, err
	}
	if got := aws.ToString(out.KeyId); got != keyARN {
		clear(out.Plaintext)
		return nil, fmt.Errorf("backup: KMS unwrapped the data key with %q, not %q", got, keyARN)
	}
	return out.Plaintext, nil
}

// PutObject uploads a backup in one request, which is all the host needs to
// be allowed: no multipart upload (it would need an abort to clean up after
// itself), no HEAD to see whether the name is free. If-None-Match: * makes S3
// refuse the request rather than replace an existing object, and a bucket
// policy can refuse the host any PutObject without it, so the host can never
// overwrite a backup.
//
// At rest the object is SSE-S3 (AES256), the bucket's default, named here so
// that a change of default cannot change what the host asks for. Never SSE-KMS
// under the backup key: S3's own call to KMS carries the context aws:s3:arn,
// which a key policy that admits only service, env, purpose and ref refuses,
// and the object is already a KMS envelope.
func (a *AWS) PutObject(ctx context.Context, in PutInput) error {
	_, err := a.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               aws.String(in.Bucket),
		Key:                  aws.String(in.Key),
		Body:                 in.Body,
		ContentLength:        aws.Int64(in.Size),
		ContentType:          aws.String("application/octet-stream"),
		ChecksumSHA256:       aws.String(base64.StdEncoding.EncodeToString(in.SHA256)),
		IfNoneMatch:          aws.String("*"),
		ServerSideEncryption: s3types.ServerSideEncryptionAes256,
	})
	return err
}

// GetObject downloads a backup. S3 removes its own at-rest layer (SSE-S3);
// the backup's encryption is untouched.
func (a *AWS) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := a.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}
