// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/YakDriver/regexache"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hashicorp/aws-sdk-go-base/v2/tfawserr"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/customdiff"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/enum"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/sdkdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/flex"
	"github.com/hashicorp/terraform-provider-aws/internal/service/kms"
	tftags "github.com/hashicorp/terraform-provider-aws/internal/tags"
	"github.com/hashicorp/terraform-provider-aws/internal/tfresource"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
	"github.com/hashicorp/terraform-provider-aws/names"
	"github.com/mitchellh/go-homedir"
)

// @SDKResource("aws_s3_object", name="Object")
// @Tags
func ResourceObject() *schema.Resource {
	return &schema.Resource{
		CreateWithoutTimeout: resourceObjectCreate,
		ReadWithoutTimeout:   resourceObjectRead,
		UpdateWithoutTimeout: resourceObjectUpdate,
		DeleteWithoutTimeout: resourceObjectDelete,

		Importer: &schema.ResourceImporter{
			StateContext: resourceObjectImport,
		},

		CustomizeDiff: customdiff.Sequence(
			resourceObjectCustomizeDiff,
			verify.SetTagsDiff,
		),

		Schema: map[string]*schema.Schema{
			"acl": {
				Type:             schema.TypeString,
				Optional:         true,
				Computed:         true,
				ValidateDiagFunc: enum.Validate[types.ObjectCannedACL](),
			},
			"bucket": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.NoZeroValues,
			},
			"bucket_key_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				Computed: true,
			},
			"cache_control": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"checksum_algorithm": {
				Type:             schema.TypeString,
				Optional:         true,
				ValidateDiagFunc: enum.Validate[types.ChecksumAlgorithm](),
			},
			"checksum_crc32": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"checksum_crc32c": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"checksum_sha1": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"checksum_sha256": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"content": {
				Type:          schema.TypeString,
				Optional:      true,
				ConflictsWith: []string{"source", "content_base64"},
			},
			"content_base64": {
				Type:          schema.TypeString,
				Optional:      true,
				ConflictsWith: []string{"source", "content"},
			},
			"content_disposition": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"content_encoding": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"content_language": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"content_type": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},
			"etag": {
				Type: schema.TypeString,
				// This will conflict with SSE-C and SSE-KMS encryption and multi-part upload
				// if/when it's actually implemented. The Etag then won't match raw-file MD5.
				// See http://docs.aws.amazon.com/AmazonS3/latest/API/RESTCommonResponseHeaders.html
				Optional:      true,
				Computed:      true,
				ConflictsWith: []string{"kms_key_id"},
			},
			"force_destroy": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"key": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.NoZeroValues,
			},
			"kms_key_id": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: verify.ValidARN,
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					// ignore diffs where the user hasn't specified a kms_key_id but the bucket has a default KMS key configured
					if new == "" && d.Get("server_side_encryption") == types.ServerSideEncryptionAwsKms {
						return true
					}
					return false
				},
			},
			"metadata": {
				Type:         schema.TypeMap,
				Optional:     true,
				Elem:         &schema.Schema{Type: schema.TypeString},
				ValidateFunc: validateMetadataIsLowerCase,
			},
			"object_lock_legal_hold_status": {
				Type:             schema.TypeString,
				Optional:         true,
				ValidateDiagFunc: enum.Validate[types.ObjectLockLegalHoldStatus](),
			},
			"object_lock_mode": {
				Type:             schema.TypeString,
				Optional:         true,
				ValidateDiagFunc: enum.Validate[types.ObjectLockMode](),
			},
			"object_lock_retain_until_date": {
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.IsRFC3339Time,
			},
			"server_side_encryption": {
				Type:             schema.TypeString,
				Optional:         true,
				Computed:         true,
				ValidateDiagFunc: enum.Validate[types.ServerSideEncryption](),
			},
			"source": {
				Type:          schema.TypeString,
				Optional:      true,
				ConflictsWith: []string{"content", "content_base64"},
			},
			"source_hash": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"storage_class": {
				Type:             schema.TypeString,
				Optional:         true,
				Computed:         true,
				ValidateDiagFunc: enum.Validate[types.ObjectStorageClass](),
			},
			names.AttrTags:    tftags.TagsSchema(),
			names.AttrTagsAll: tftags.TagsSchemaComputed(),
			"version_id": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"website_redirect": {
				Type:     schema.TypeString,
				Optional: true,
			},
		},
	}
}

func resourceObjectCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	return append(diags, resourceObjectUpload(ctx, d, meta)...)
}

func resourceObjectRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).S3Client(ctx)

	bucket := d.Get("bucket").(string)
	key := sdkv1CompatibleCleanKey(d.Get("key").(string))
	output, err := findObjectByBucketAndKey(ctx, conn, bucket, key, "", d.Get("checksum_algorithm").(string))

	if !d.IsNewResource() && tfresource.NotFound(err) {
		log.Printf("[WARN] S3 Object (%s) not found, removing from state", d.Id())
		d.SetId("")
		return diags
	}

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "reading S3 Object (%s): %s", d.Id(), err)
	}

	d.Set("bucket_key_enabled", output.BucketKeyEnabled)
	d.Set("cache_control", output.CacheControl)
	d.Set("checksum_crc32", output.ChecksumCRC32)
	d.Set("checksum_crc32c", output.ChecksumCRC32C)
	d.Set("checksum_sha1", output.ChecksumSHA1)
	d.Set("checksum_sha256", output.ChecksumSHA256)
	d.Set("content_disposition", output.ContentDisposition)
	d.Set("content_encoding", output.ContentEncoding)
	d.Set("content_language", output.ContentLanguage)
	d.Set("content_type", output.ContentType)
	// See https://forums.aws.amazon.com/thread.jspa?threadID=44003
	d.Set("etag", strings.Trim(aws.ToString(output.ETag), `"`))
	d.Set("metadata", output.Metadata)
	d.Set("object_lock_legal_hold_status", output.ObjectLockLegalHoldStatus)
	d.Set("object_lock_mode", output.ObjectLockMode)
	d.Set("object_lock_retain_until_date", flattenObjectDate(output.ObjectLockRetainUntilDate))
	d.Set("server_side_encryption", output.ServerSideEncryption)
	// The "STANDARD" (which is also the default) storage
	// class when set would not be included in the results.
	d.Set("storage_class", types.ObjectStorageClassStandard)
	if output.StorageClass != "" {
		d.Set("storage_class", output.StorageClass)
	}
	d.Set("version_id", output.VersionId)
	d.Set("website_redirect", output.WebsiteRedirectLocation)

	if err := resourceObjectSetKMS(ctx, d, meta, output.SSEKMSKeyId); err != nil {
		return sdkdiag.AppendFromErr(diags, err)
	}

	tags, err := ObjectListTags(ctx, conn, bucket, key)

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "listing tags for S3 Bucket (%s) Object (%s): %s", bucket, key, err)
	}

	setTagsOut(ctx, Tags(tags))

	return diags
}

func resourceObjectUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	if hasObjectContentChanges(d) {
		return append(diags, resourceObjectUpload(ctx, d, meta)...)
	}

	conn := meta.(*conns.AWSClient).S3Client(ctx)

	bucket := d.Get("bucket").(string)
	key := sdkv1CompatibleCleanKey(d.Get("key").(string))

	if d.HasChange("acl") {
		input := &s3.PutObjectAclInput{
			ACL:    types.ObjectCannedACL(d.Get("acl").(string)),
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}

		_, err := conn.PutObjectAcl(ctx, input)

		if err != nil {
			return sdkdiag.AppendErrorf(diags, "putting S3 Object (%s) ACL: %s", d.Id(), err)
		}
	}

	if d.HasChange("object_lock_legal_hold_status") {
		input := &s3.PutObjectLegalHoldInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			LegalHold: &types.ObjectLockLegalHold{
				Status: types.ObjectLockLegalHoldStatus(d.Get("object_lock_legal_hold_status").(string)),
			},
		}

		_, err := conn.PutObjectLegalHold(ctx, input)

		if err != nil {
			return sdkdiag.AppendErrorf(diags, "putting S3 Object (%s) legal hold: %s", d.Id(), err)
		}
	}

	if d.HasChanges("object_lock_mode", "object_lock_retain_until_date") {
		input := &s3.PutObjectRetentionInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Retention: &types.ObjectLockRetention{
				Mode:            types.ObjectLockRetentionMode(d.Get("object_lock_mode").(string)),
				RetainUntilDate: expandObjectDate(d.Get("object_lock_retain_until_date").(string)),
			},
		}

		// Bypass required to lower or clear retain-until date.
		if d.HasChange("object_lock_retain_until_date") {
			oraw, nraw := d.GetChange("object_lock_retain_until_date")
			o, n := expandObjectDate(oraw.(string)), expandObjectDate(nraw.(string))

			if n == nil || (o != nil && n.Before(*o)) {
				input.BypassGovernanceRetention = true
			}
		}

		_, err := conn.PutObjectRetention(ctx, input)

		if err != nil {
			return sdkdiag.AppendErrorf(diags, "putting S3 Object (%s) retention: %s", d.Id(), err)
		}
	}

	if d.HasChange("tags_all") {
		o, n := d.GetChange("tags_all")

		if err := ObjectUpdateTags(ctx, conn, bucket, key, o, n); err != nil {
			return sdkdiag.AppendErrorf(diags, "updating tags: %s", err)
		}
	}

	return append(diags, resourceObjectRead(ctx, d, meta)...)
}

func resourceObjectDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).S3Client(ctx)

	bucket := d.Get("bucket").(string)
	key := sdkv1CompatibleCleanKey(d.Get("key").(string))

	var err error
	if _, ok := d.GetOk("version_id"); ok {
		_, err = deleteAllObjectVersions(ctx, conn, bucket, key, d.Get("force_destroy").(bool), false)
	} else {
		err = deleteObjectVersion(ctx, conn, bucket, key, "", false)
	}

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "deleting S3 Bucket (%s) Object (%s): %s", bucket, key, err)
	}

	return diags
}

func resourceObjectImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	id := d.Id()
	id = strings.TrimPrefix(id, "s3://")
	parts := strings.Split(id, "/")

	if len(parts) < 2 {
		return []*schema.ResourceData{d}, fmt.Errorf("id %s should be in format <bucket>/<key> or s3://<bucket>/<key>", id)
	}

	bucket := parts[0]
	key := strings.Join(parts[1:], "/")

	d.SetId(key)
	d.Set("bucket", bucket)
	d.Set("key", key)

	return []*schema.ResourceData{d}, nil
}

func resourceObjectUpload(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).S3Client(ctx)
	uploader := manager.NewUploader(conn)
	defaultTagsConfig := meta.(*conns.AWSClient).DefaultTagsConfig
	tags := defaultTagsConfig.MergeTags(tftags.New(ctx, d.Get("tags").(map[string]interface{})))

	var body io.ReadSeeker

	if v, ok := d.GetOk("source"); ok {
		source := v.(string)
		path, err := homedir.Expand(source)
		if err != nil {
			return sdkdiag.AppendErrorf(diags, "expanding homedir in source (%s): %s", source, err)
		}
		file, err := os.Open(path)
		if err != nil {
			return sdkdiag.AppendErrorf(diags, "opening S3 object source (%s): %s", path, err)
		}

		body = file
		defer func() {
			err := file.Close()
			if err != nil {
				log.Printf("[WARN] Error closing S3 object source (%s): %s", path, err)
			}
		}()
	} else if v, ok := d.GetOk("content"); ok {
		content := v.(string)
		body = bytes.NewReader([]byte(content))
	} else if v, ok := d.GetOk("content_base64"); ok {
		content := v.(string)
		// We can't do streaming decoding here (with base64.NewDecoder) because
		// the AWS SDK requires an io.ReadSeeker but a base64 decoder can't seek.
		contentRaw, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return sdkdiag.AppendErrorf(diags, "decoding content_base64: %s", err)
		}
		body = bytes.NewReader(contentRaw)
	} else {
		body = bytes.NewReader([]byte{})
	}

	input := &s3.PutObjectInput{
		Body:   body,
		Bucket: aws.String(d.Get("bucket").(string)),
		Key:    aws.String(sdkv1CompatibleCleanKey(d.Get("key").(string))),
	}

	if v, ok := d.GetOk("acl"); ok {
		input.ACL = types.ObjectCannedACL(v.(string))
	}

	if v, ok := d.GetOk("bucket_key_enabled"); ok {
		input.BucketKeyEnabled = v.(bool)
	}

	if v, ok := d.GetOk("cache_control"); ok {
		input.CacheControl = aws.String(v.(string))
	}

	if v, ok := d.GetOk("checksum_algorithm"); ok {
		input.ChecksumAlgorithm = types.ChecksumAlgorithm(v.(string))
	}

	if v, ok := d.GetOk("content_disposition"); ok {
		input.ContentDisposition = aws.String(v.(string))
	}

	if v, ok := d.GetOk("content_encoding"); ok {
		input.ContentEncoding = aws.String(v.(string))
	}

	if v, ok := d.GetOk("content_language"); ok {
		input.ContentLanguage = aws.String(v.(string))
	}

	if v, ok := d.GetOk("content_type"); ok {
		input.ContentType = aws.String(v.(string))
	}

	if v, ok := d.GetOk("kms_key_id"); ok {
		input.SSEKMSKeyId = aws.String(v.(string))
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
	}

	if v, ok := d.GetOk("metadata"); ok {
		input.Metadata = flex.ExpandStringValueMap(v.(map[string]interface{}))
	}

	if v, ok := d.GetOk("object_lock_legal_hold_status"); ok {
		input.ObjectLockLegalHoldStatus = types.ObjectLockLegalHoldStatus(v.(string))
	}

	if v, ok := d.GetOk("object_lock_mode"); ok {
		input.ObjectLockMode = types.ObjectLockMode(v.(string))
	}

	if v, ok := d.GetOk("object_lock_retain_until_date"); ok {
		input.ObjectLockRetainUntilDate = expandObjectDate(v.(string))
	}

	if v, ok := d.GetOk("server_side_encryption"); ok {
		input.ServerSideEncryption = types.ServerSideEncryption(v.(string))
	}

	if v, ok := d.GetOk("storage_class"); ok {
		input.StorageClass = types.StorageClass(v.(string))
	}

	if len(tags) > 0 {
		// The tag-set must be encoded as URL Query parameters.
		input.Tagging = aws.String(tags.IgnoreAWS().URLEncode())
	}

	if v, ok := d.GetOk("website_redirect"); ok {
		input.WebsiteRedirectLocation = aws.String(v.(string))
	}

	if (input.ObjectLockLegalHoldStatus != "" || input.ObjectLockMode != "" || input.ObjectLockRetainUntilDate != nil) && input.ChecksumAlgorithm == "" {
		// "Content-MD5 OR x-amz-checksum- HTTP header is required for Put Object requests with Object Lock parameters".
		// AWS SDK for Go v1 transparently added a Content-MD4 header.
		input.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
	}

	if _, err := uploader.Upload(ctx, input); err != nil {
		return sdkdiag.AppendErrorf(diags, "uploading S3 Object (%s) to Bucket (%s): %s", aws.ToString(input.Key), aws.ToString(input.Bucket), err)
	}

	if d.IsNewResource() {
		d.SetId(d.Get("key").(string))
	}

	return append(diags, resourceObjectRead(ctx, d, meta)...)
}

func resourceObjectSetKMS(ctx context.Context, d *schema.ResourceData, meta interface{}, sseKMSKeyId *string) error {
	// Only set non-default KMS key ID (one that doesn't match default)
	if sseKMSKeyId != nil {
		// retrieve S3 KMS Default Master Key
		conn := meta.(*conns.AWSClient).KMSConn(ctx)
		keyMetadata, err := kms.FindKeyByID(ctx, conn, DefaultKMSKeyAlias)
		if err != nil {
			return fmt.Errorf("Failed to describe default S3 KMS key (%s): %s", DefaultKMSKeyAlias, err)
		}

		if kmsKeyID := aws.ToString(sseKMSKeyId); kmsKeyID != aws.ToString(keyMetadata.Arn) {
			log.Printf("[DEBUG] S3 object is encrypted using a non-default KMS Key ID: %s", kmsKeyID)
			d.Set("kms_key_id", sseKMSKeyId)
		}
	}

	return nil
}

func validateMetadataIsLowerCase(v interface{}, k string) (ws []string, errors []error) {
	value := v.(map[string]interface{})

	for k := range value {
		if k != strings.ToLower(k) {
			errors = append(errors, fmt.Errorf(
				"Metadata must be lowercase only. Offending key: %q", k))
		}
	}
	return
}

func resourceObjectCustomizeDiff(_ context.Context, d *schema.ResourceDiff, meta interface{}) error {
	if hasObjectContentChanges(d) {
		return d.SetNewComputed("version_id")
	}

	if d.HasChange("source_hash") {
		d.SetNewComputed("version_id")
		d.SetNewComputed("etag")
	}

	return nil
}

func hasObjectContentChanges(d verify.ResourceDiffer) bool {
	for _, key := range []string{
		"bucket_key_enabled",
		"cache_control",
		"checksum_algorithm",
		"content_base64",
		"content_disposition",
		"content_encoding",
		"content_language",
		"content_type",
		"content",
		"etag",
		"kms_key_id",
		"metadata",
		"server_side_encryption",
		"source",
		"source_hash",
		"storage_class",
		"website_redirect",
	} {
		if d.HasChange(key) {
			return true
		}
	}
	return false
}

func findObjectByBucketAndKey(ctx context.Context, conn *s3.Client, bucket, key, etag, checksumAlgorithm string) (*s3.HeadObjectOutput, error) {
	input := &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if checksumAlgorithm != "" {
		input.ChecksumMode = types.ChecksumModeEnabled
	}
	if etag != "" {
		input.IfMatch = aws.String(etag)
	}

	return findObject(ctx, conn, input)
}

func findObject(ctx context.Context, conn *s3.Client, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	output, err := conn.HeadObject(ctx, input)

	if tfawserr.ErrHTTPStatusCodeEquals(err, http.StatusNotFound) {
		return nil, &retry.NotFoundError{
			LastError:   err,
			LastRequest: input,
		}
	}

	if err != nil {
		return nil, err
	}

	if output == nil {
		return nil, tfresource.NewEmptyResultError(input)
	}

	return output, nil
}

// deleteAllObjectVersions deletes all versions of a specified key from an S3 bucket.
// If key is empty then all versions of all objects are deleted.
// Set force to true to override any S3 object lock protections on object lock enabled buckets.
// Returns the number of objects deleted.
func deleteAllObjectVersions(ctx context.Context, conn *s3.Client, bucketName, key string, force, ignoreObjectErrors bool) (int64, error) {
	var nObjects int64

	input := &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucketName),
	}
	if key != "" {
		input.Prefix = aws.String(key)
	}

	var lastErr error

	pages := s3.NewListObjectVersionsPaginator(conn, input)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)

		if tfawserr.ErrCodeEquals(err, errCodeNoSuchBucket) {
			break
		}

		if err != nil {
			return nObjects, err
		}

		for _, objectVersion := range page.Versions {
			objectKey := aws.ToString(objectVersion.Key)
			objectVersionID := aws.ToString(objectVersion.VersionId)

			if key != "" && key != objectKey {
				continue
			}

			err := deleteObjectVersion(ctx, conn, bucketName, objectKey, objectVersionID, force)

			if err == nil {
				nObjects++
			}

			if tfawserr.ErrCodeEquals(err, errCodeAccessDenied) && force {
				// Remove any legal hold.
				input := &s3.HeadObjectInput{
					Bucket:    aws.String(bucketName),
					Key:       aws.String(objectKey),
					VersionId: aws.String(objectVersionID),
				}

				output, err := conn.HeadObject(ctx, input)

				if err != nil {
					log.Printf("[ERROR] Getting S3 Bucket (%s) Object (%s) Version (%s) metadata: %s", bucketName, objectKey, objectVersionID, err)
					lastErr = err
					continue
				}

				if output.ObjectLockLegalHoldStatus == types.ObjectLockLegalHoldStatusOn {
					input := &s3.PutObjectLegalHoldInput{
						Bucket: aws.String(bucketName),
						Key:    aws.String(objectKey),
						LegalHold: &types.ObjectLockLegalHold{
							Status: types.ObjectLockLegalHoldStatusOff,
						},
						VersionId: aws.String(objectVersionID),
					}

					_, err := conn.PutObjectLegalHold(ctx, input)

					if err != nil {
						log.Printf("[ERROR] Putting S3 Bucket (%s) Object (%s) Version(%s) legal hold: %s", bucketName, objectKey, objectVersionID, err)
						lastErr = err
						continue
					}

					// Attempt to delete again.
					err = deleteObjectVersion(ctx, conn, bucketName, objectKey, objectVersionID, force)

					if err != nil {
						lastErr = err
					} else {
						nObjects++
					}

					continue
				}

				// AccessDenied for another reason.
				lastErr = fmt.Errorf("deleting S3 Bucket (%s) Object (%s) Version (%s): %w", bucketName, objectKey, objectVersionID, err)
				continue
			}

			if err != nil {
				lastErr = err
			}
		}
	}

	if lastErr != nil {
		if !ignoreObjectErrors {
			return nObjects, fmt.Errorf("deleting at least one S3 Object version, last error: %w", lastErr)
		}

		lastErr = nil
	}

	pages = s3.NewListObjectVersionsPaginator(conn, input)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)

		if tfawserr.ErrCodeEquals(err, errCodeNoSuchBucket) {
			break
		}

		if err != nil {
			return nObjects, err
		}

		for _, deleteMarker := range page.DeleteMarkers {
			deleteMarkerKey := aws.ToString(deleteMarker.Key)
			deleteMarkerVersionID := aws.ToString(deleteMarker.VersionId)

			if key != "" && key != deleteMarkerKey {
				continue
			}

			// Delete markers have no object lock protections.
			err := deleteObjectVersion(ctx, conn, bucketName, deleteMarkerKey, deleteMarkerVersionID, false)

			if err != nil {
				lastErr = err
			} else {
				nObjects++
			}
		}
	}

	if lastErr != nil {
		if !ignoreObjectErrors {
			return nObjects, fmt.Errorf("deleting at least one S3 Object delete marker, last error: %w", lastErr)
		}
	}

	return nObjects, nil
}

// deleteObjectVersion deletes a specific object version.
// Set force to true to override any S3 object lock protections.
func deleteObjectVersion(ctx context.Context, conn *s3.Client, b, k, v string, force bool) error {
	input := &s3.DeleteObjectInput{
		Bucket: aws.String(b),
		Key:    aws.String(k),
	}

	if v != "" {
		input.VersionId = aws.String(v)
	}
	if force {
		input.BypassGovernanceRetention = true
	}

	log.Printf("[INFO] Deleting S3 Bucket (%s) Object (%s) Version (%s)", b, k, v)
	_, err := conn.DeleteObject(ctx, input)

	if err != nil {
		log.Printf("[WARN] Deleting S3 Bucket (%s) Object (%s) Version (%s): %s", b, k, v, err)
	}

	if tfawserr.ErrCodeEquals(err, errCodeNoSuchBucket, errCodeNoSuchKey) {
		return nil
	}

	return err
}

func expandObjectDate(v string) *time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}

	return aws.Time(t)
}

func flattenObjectDate(t *time.Time) string {
	if t == nil {
		return ""
	}

	return t.Format(time.RFC3339)
}

// sdkv1CompatibleCleanKey returns an AWS SDK for Go v1 compatible clean key.
// DisableRestProtocolURICleaning was false on the standard S3Conn, so to ensure backwards
// compatibility we must "clean" the configured key before passing to AWS SDK for Go v2 APIs.
// See https://docs.aws.amazon.com/sdk-for-go/api/service/s3/#hdr-Automatic_URI_cleaning.
// See https://github.com/aws/aws-sdk-go/blob/cf903c8c543034654bb8f53b5f9d6454fdb2117f/private/protocol/rest/build.go#L247-L258.
func sdkv1CompatibleCleanKey(key string) string {
	// We are effectively ignoring all leading '/'s and treating multiple '/'s as a single '/'.
	key = strings.TrimLeft(key, "/")
	key = regexache.MustCompile(`/+`).ReplaceAllString(key, "/")
	return key
}
