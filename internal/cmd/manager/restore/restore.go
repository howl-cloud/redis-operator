// Package restore provides bootstrap restore logic for init containers.
package restore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	defaultBackupFilename = "dump.rdb"
	defaultAWSRegion      = "us-east-1"
	defaultAOFDirectory   = "appendonlydir"
	defaultAOFManifest    = "appendonly.aof.manifest"
	maxExtractedBytes     = int64(32 << 30) // 32 GiB hard cap
	restoredAOFDirMode    = os.FileMode(0o777)
	restoredAOFFileMode   = os.FileMode(0o666)
)

// Run restores a backup object from S3 into the given data directory.
func Run(ctx context.Context, clusterName, backupName, backupNamespace, dataDir string) error {
	if clusterName == "" {
		return fmt.Errorf("cluster name is required")
	}
	if backupName == "" {
		return fmt.Errorf("backup name is required")
	}
	if backupNamespace == "" {
		return fmt.Errorf("backup namespace is required")
	}
	if dataDir == "" {
		return fmt.Errorf("data directory is required")
	}

	k8sClient, err := newKubernetesClient()
	if err != nil {
		return fmt.Errorf("creating Kubernetes client: %w", err)
	}

	var backup redisv1.RedisBackup
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      backupName,
		Namespace: backupNamespace,
	}, &backup); err != nil {
		return fmt.Errorf("fetching RedisBackup %s/%s: %w", backupNamespace, backupName, err)
	}

	if backup.Status.Phase != redisv1.BackupPhaseCompleted {
		return fmt.Errorf("RedisBackup %s/%s is not completed (phase=%s)", backupNamespace, backupName, backup.Status.Phase)
	}
	if backup.Spec.Destination == nil || backup.Spec.Destination.S3 == nil {
		return fmt.Errorf("RedisBackup %s/%s has no S3 destination", backupNamespace, backupName)
	}
	if backup.Status.BackupPath == "" {
		return fmt.Errorf("RedisBackup %s/%s has empty status.backupPath", backupNamespace, backupName)
	}
	artifactType, err := resolveArtifactType(&backup)
	if err != nil {
		return fmt.Errorf("determining backup artifact type for RedisBackup %s/%s: %w", backupNamespace, backupName, err)
	}

	bucket, key, err := resolveBackupLocation(backup.Spec.Destination.S3.Bucket, backup.Status.BackupPath)
	if err != nil {
		return fmt.Errorf("resolving backup location: %w", err)
	}

	var cluster redisv1.RedisCluster
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      clusterName,
		Namespace: backupNamespace,
	}, &cluster); err != nil {
		return fmt.Errorf("fetching RedisCluster %s/%s: %w", backupNamespace, clusterName, err)
	}
	if cluster.Spec.BackupCredentialsSecret == nil {
		return fmt.Errorf("RedisCluster %s/%s has no backupCredentialsSecret configured", backupNamespace, cluster.Name)
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.BackupCredentialsSecret.Name,
		Namespace: backupNamespace,
	}, &secret); err != nil {
		return fmt.Errorf("fetching backup credentials secret %s/%s: %w", backupNamespace, cluster.Spec.BackupCredentialsSecret.Name, err)
	}

	accessKeyID, secretAccessKey, sessionToken, err := readAWSCredentials(&secret)
	if err != nil {
		return fmt.Errorf("reading AWS credentials from secret %s/%s: %w", backupNamespace, secret.Name, err)
	}

	s3Client, err := newS3Client(ctx, backup.Spec.Destination.S3, accessKeyID, secretAccessKey, sessionToken)
	if err != nil {
		return fmt.Errorf("creating S3 client: %w", err)
	}

	switch artifactType {
	case redisv1.BackupArtifactTypeRDB:
		targetPath := filepath.Join(dataDir, defaultBackupFilename)
		if err := downloadObjectToFile(ctx, s3Client, bucket, key, targetPath); err != nil {
			return fmt.Errorf("downloading s3://%s/%s to %s: %w", bucket, key, targetPath, err)
		}
	case redisv1.BackupArtifactTypeAOFArchive:
		if err := downloadAndExtractAOFArchive(ctx, s3Client, bucket, key, dataDir); err != nil {
			return fmt.Errorf("restoring AOF archive from s3://%s/%s: %w", bucket, key, err)
		}
	default:
		return fmt.Errorf("unsupported artifact type %q", artifactType)
	}

	return nil
}

func resolveArtifactType(backup *redisv1.RedisBackup) (redisv1.BackupArtifactType, error) {
	if backup.Status.ArtifactType != "" {
		switch backup.Status.ArtifactType {
		case redisv1.BackupArtifactTypeRDB, redisv1.BackupArtifactTypeAOFArchive:
			return backup.Status.ArtifactType, nil
		default:
			return "", fmt.Errorf("unknown status.artifactType %q", backup.Status.ArtifactType)
		}
	}

	method := backup.Spec.Method
	if method == "" {
		method = redisv1.BackupMethodRDB
	}

	switch method {
	case redisv1.BackupMethodRDB:
		return redisv1.BackupArtifactTypeRDB, nil
	case redisv1.BackupMethodAOF:
		return redisv1.BackupArtifactTypeAOFArchive, nil
	default:
		return "", fmt.Errorf("unsupported backup method %q", method)
	}
}

func downloadAndExtractAOFArchive(ctx context.Context, s3Client *s3.Client, bucket, key, dataDir string) error {
	archiveFile, err := os.CreateTemp(dataDir, "aof-archive-*.tar.gz")
	if err != nil {
		return fmt.Errorf("creating temporary AOF archive file: %w", err)
	}
	archivePath := archiveFile.Name()
	if err := archiveFile.Close(); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("closing temporary archive file %s: %w", archivePath, err)
	}
	defer func() { _ = os.Remove(archivePath) }()

	if err := downloadObjectToFile(ctx, s3Client, bucket, key, archivePath); err != nil {
		return fmt.Errorf("downloading archive to %s: %w", archivePath, err)
	}

	if err := extractTarGz(archivePath, dataDir); err != nil {
		return fmt.Errorf("extracting AOF archive %s: %w", archivePath, err)
	}

	return nil
}

func extractTarGz(archivePath, dataDir string) error {
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("stat archive %s: %w", archivePath, err)
	}

	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive %s: %w", archivePath, err)
	}
	defer archive.Close() //nolint:errcheck // read-only close is best effort

	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("creating gzip reader for %s: %w", archivePath, err)
	}
	defer gzipReader.Close() //nolint:errcheck // close is best effort

	tarReader := tar.NewReader(gzipReader)
	targetDir := filepath.Join(dataDir, defaultAOFDirectory)
	stagingDir := targetDir + ".restore-tmp"
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("cleaning staging directory %s: %w", stagingDir, err)
	}
	if err := os.MkdirAll(stagingDir, restoredAOFDirMode); err != nil {
		return fmt.Errorf("creating staging directory %s: %w", stagingDir, err)
	}
	if err := os.Chmod(stagingDir, restoredAOFDirMode); err != nil {
		return fmt.Errorf("setting permissions on staging directory %s: %w", stagingDir, err)
	}

	extractLimit := info.Size()*100 + (64 << 20) // allow compression expansion with a bounded ratio.
	if extractLimit > maxExtractedBytes {
		extractLimit = maxExtractedBytes
	}
	var extractedBytes int64

	cleanupStaging := func() {
		_ = os.RemoveAll(stagingDir)
	}

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanupStaging()
			return fmt.Errorf("reading tar entry: %w", err)
		}

		entryPath, err := sanitizeArchivePath(stagingDir, header.Name)
		if err != nil {
			cleanupStaging()
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(entryPath, restoredAOFDirMode); err != nil {
				cleanupStaging()
				return fmt.Errorf("creating directory %s: %w", entryPath, err)
			}
			if err := os.Chmod(entryPath, restoredAOFDirMode); err != nil {
				cleanupStaging()
				return fmt.Errorf("setting permissions on directory %s: %w", entryPath, err)
			}
		case tar.TypeReg:
			parentDir := filepath.Dir(entryPath)
			if err := os.MkdirAll(parentDir, restoredAOFDirMode); err != nil {
				cleanupStaging()
				return fmt.Errorf("creating parent directory for %s: %w", entryPath, err)
			}
			if err := os.Chmod(parentDir, restoredAOFDirMode); err != nil {
				cleanupStaging()
				return fmt.Errorf("setting permissions on parent directory %s: %w", parentDir, err)
			}
			output, err := os.OpenFile(entryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, restoredAOFFileMode)
			if err != nil {
				cleanupStaging()
				return fmt.Errorf("creating extracted file %s: %w", entryPath, err)
			}

			written, copyErr := io.Copy(output, io.LimitReader(tarReader, header.Size))
			closeErr := output.Close()
			if copyErr != nil {
				cleanupStaging()
				return fmt.Errorf("extracting file %s: %w", entryPath, copyErr)
			}
			if closeErr != nil {
				cleanupStaging()
				return fmt.Errorf("closing extracted file %s: %w", entryPath, closeErr)
			}
			if written != header.Size {
				cleanupStaging()
				return fmt.Errorf("extracted file %s truncated (wrote %d bytes, expected %d)", entryPath, written, header.Size)
			}
			if err := os.Chmod(entryPath, restoredAOFFileMode); err != nil {
				cleanupStaging()
				return fmt.Errorf("setting permissions on extracted file %s: %w", entryPath, err)
			}

			extractedBytes += written
			if extractedBytes > extractLimit {
				cleanupStaging()
				return fmt.Errorf("archive expands to %d bytes, exceeding limit %d", extractedBytes, extractLimit)
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// PAX metadata entries are consumed by the tar reader and need no extraction.
			continue
		default:
			cleanupStaging()
			return fmt.Errorf("unsupported tar entry type %d for %q", header.Typeflag, header.Name)
		}
	}

	manifestPath := filepath.Join(stagingDir, defaultAOFManifest)
	if _, err := os.Stat(manifestPath); err != nil {
		cleanupStaging()
		return fmt.Errorf("expected AOF manifest %s is missing: %w", manifestPath, err)
	}

	if err := os.RemoveAll(targetDir); err != nil {
		cleanupStaging()
		return fmt.Errorf("removing old AOF directory %s: %w", targetDir, err)
	}
	if err := os.Rename(stagingDir, targetDir); err != nil {
		cleanupStaging()
		return fmt.Errorf("promoting staging directory %s to %s: %w", stagingDir, targetDir, err)
	}

	return nil
}

func sanitizeArchivePath(baseDir, archiveEntry string) (string, error) {
	cleanEntry := path.Clean(strings.TrimSpace(archiveEntry))
	if cleanEntry == "." || cleanEntry == "" {
		return "", fmt.Errorf("archive contains empty path entry")
	}
	if strings.HasPrefix(cleanEntry, "/") || cleanEntry == ".." || strings.HasPrefix(cleanEntry, "../") {
		return "", fmt.Errorf("archive entry %q escapes extraction root", archiveEntry)
	}

	targetPath := filepath.Join(baseDir, filepath.FromSlash(cleanEntry))
	cleanBase := filepath.Clean(baseDir)
	cleanTarget := filepath.Clean(targetPath)
	if cleanTarget != cleanBase && !strings.HasPrefix(cleanTarget, cleanBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes extraction root", archiveEntry)
	}

	return cleanTarget, nil
}

func newKubernetesClient() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return k8sClient, nil
}

func newS3Client(ctx context.Context, destination *redisv1.S3Destination, accessKeyID, secretAccessKey, sessionToken string) (*s3.Client, error) {
	region := destination.Region
	if region == "" {
		region = defaultAWSRegion
	}

	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if destination.Endpoint != "" {
			options.BaseEndpoint = aws.String(destination.Endpoint)
			// Most S3-compatible object stores require path-style addressing.
			options.UsePathStyle = true
		}
	})

	return client, nil
}

func readAWSCredentials(secret *corev1.Secret) (string, string, string, error) {
	accessKeyID := secretValue(
		secret,
		"AWS_ACCESS_KEY_ID",
		"aws_access_key_id",
		"accessKeyId",
		"access_key_id",
	)
	secretAccessKey := secretValue(
		secret,
		"AWS_SECRET_ACCESS_KEY",
		"aws_secret_access_key",
		"secretAccessKey",
		"secret_access_key",
	)
	sessionToken := secretValue(
		secret,
		"AWS_SESSION_TOKEN",
		"aws_session_token",
		"sessionToken",
		"session_token",
	)

	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", "", fmt.Errorf("secret must include access key and secret key (supported keys: AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)")
	}

	return accessKeyID, secretAccessKey, sessionToken, nil
}

func secretValue(secret *corev1.Secret, keys ...string) string {
	for _, key := range keys {
		value, ok := secret.Data[key]
		if !ok {
			continue
		}
		asString := strings.TrimSpace(string(value))
		if asString != "" {
			return asString
		}
	}
	return ""
}

func resolveBackupLocation(configuredBucket, backupPath string) (string, string, error) {
	trimmedPath := strings.TrimSpace(backupPath)
	if trimmedPath == "" {
		return "", "", fmt.Errorf("backup path is empty")
	}

	if strings.HasPrefix(trimmedPath, "s3://") {
		parsedURL, err := url.Parse(trimmedPath)
		if err != nil {
			return "", "", fmt.Errorf("parsing s3 backup path %q: %w", trimmedPath, err)
		}
		if parsedURL.Host == "" {
			return "", "", fmt.Errorf("s3 backup path %q is missing bucket", trimmedPath)
		}

		bucket := configuredBucket
		if bucket == "" {
			bucket = parsedURL.Host
		}
		if configuredBucket != "" && configuredBucket != parsedURL.Host {
			return "", "", fmt.Errorf("backup path bucket %q does not match configured bucket %q", parsedURL.Host, configuredBucket)
		}

		key := strings.TrimPrefix(parsedURL.Path, "/")
		if key == "" {
			return "", "", fmt.Errorf("s3 backup path %q is missing object key", trimmedPath)
		}
		return bucket, key, nil
	}

	if configuredBucket == "" {
		return "", "", fmt.Errorf("backup path %q is not an s3:// URL and no destination bucket is configured", trimmedPath)
	}

	key := strings.TrimPrefix(trimmedPath, "/")
	if key == "" {
		return "", "", fmt.Errorf("backup path %q is missing object key", trimmedPath)
	}

	return configuredBucket, key, nil
}

func downloadObjectToFile(ctx context.Context, s3Client *s3.Client, bucket, key, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	response, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	tempPath := outputPath + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening output file %s: %w", tempPath, err)
	}

	if _, err := io.Copy(file, response.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("writing backup file: %w", err)
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("closing output file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, outputPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("renaming %s to %s: %w", tempPath, outputPath, err)
	}

	return nil
}
