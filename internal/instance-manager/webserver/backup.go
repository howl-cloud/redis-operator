package webserver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	defaultBackupDataDir        = "/data"
	defaultBackupCredsMountPath = "/backup-credentials"
	defaultBackupFilename       = "dump.rdb"
	defaultAOFDirName           = "appendonlydir"
	defaultAOFManifestFilename  = "appendonly.aof.manifest"
	defaultAWSRegion            = "us-east-1"
	backupPollInterval          = 250 * time.Millisecond
	backupTimeout               = 5 * time.Minute
)

type backupUploaderFunc func(
	ctx context.Context,
	backupName string,
	destination *redisv1.S3Destination,
	artifactPath string,
	artifactType redisv1.BackupArtifactType,
) (*backupResponse, error)

type backupRequest struct {
	BackupName  string                     `json:"backupName"`
	Method      redisv1.BackupMethod       `json:"method,omitempty"`
	Destination *redisv1.BackupDestination `json:"destination,omitempty"`
}

type backupResponse struct {
	ArtifactType   redisv1.BackupArtifactType `json:"artifactType"`
	BackupPath     string                     `json:"backupPath"`
	ObjectKey      string                     `json:"objectKey"`
	BackupSize     int64                      `json:"backupSize"`
	ChecksumSHA256 string                     `json:"checksumSHA256,omitempty"`
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.redisClient == nil {
		http.Error(w, "redis client is not configured", http.StatusInternalServerError)
		return
	}

	var req backupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if req.BackupName == "" {
		http.Error(w, "backupName is required", http.StatusBadRequest)
		return
	}
	if req.Destination == nil || req.Destination.S3 == nil || req.Destination.S3.Bucket == "" {
		http.Error(w, "destination.s3.bucket is required", http.StatusBadRequest)
		return
	}

	method := req.Method
	if method == "" {
		method = redisv1.BackupMethodRDB
	}

	ctx, cancel := context.WithTimeout(r.Context(), backupTimeout)
	defer cancel()

	artifactPath, artifactType, cleanup, err := s.prepareBackupArtifact(ctx, method)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("preparing backup artifact: %v", err), http.StatusInternalServerError)
		return
	}

	uploader := s.backupUploader
	if uploader == nil {
		uploader = s.uploadBackupToS3
	}

	result, err := uploader(ctx, req.BackupName, req.Destination.S3, artifactPath, artifactType)
	if err != nil {
		http.Error(w, fmt.Sprintf("uploading backup artifact: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) prepareBackupArtifact(
	ctx context.Context,
	method redisv1.BackupMethod,
) (path string, artifactType redisv1.BackupArtifactType, cleanup func(), err error) {
	switch method {
	case redisv1.BackupMethodRDB:
		if err := s.redisClient.BgSave(ctx).Err(); err != nil {
			return "", "", nil, fmt.Errorf("triggering BGSAVE: %w", err)
		}
		if err := waitForPersistenceCompletion(ctx, s.redisClient, "rdb_bgsave_in_progress", "rdb_last_bgsave_status"); err != nil {
			return "", "", nil, err
		}

		rdbPath := filepath.Join(s.backupDataDir(), defaultBackupFilename)
		if _, err := os.Stat(rdbPath); err != nil {
			return "", "", nil, fmt.Errorf("checking RDB file %s: %w", rdbPath, err)
		}

		return rdbPath, redisv1.BackupArtifactTypeRDB, nil, nil

	case redisv1.BackupMethodAOF:
		if err := s.redisClient.Do(ctx, "BGREWRITEAOF").Err(); err != nil {
			return "", "", nil, fmt.Errorf("triggering BGREWRITEAOF: %w", err)
		}
		if err := waitForPersistenceCompletion(ctx, s.redisClient, "aof_rewrite_in_progress", "aof_last_bgrewrite_status"); err != nil {
			return "", "", nil, err
		}

		appendOnlyDir := filepath.Join(s.backupDataDir(), defaultAOFDirName)
		archivePath, err := archiveDirectoryAsTarGz(appendOnlyDir, defaultAOFManifestFilename)
		if err != nil {
			return "", "", nil, err
		}
		cleanupFn := func() {
			_ = os.Remove(archivePath)
		}
		return archivePath, redisv1.BackupArtifactTypeAOFArchive, cleanupFn, nil

	default:
		return "", "", nil, fmt.Errorf("unsupported backup method %q", method)
	}
}

func waitForPersistenceCompletion(
	ctx context.Context,
	redisClient *redis.Client,
	inProgressKey string,
	lastStatusKey string,
) error {
	ticker := time.NewTicker(backupPollInterval)
	defer ticker.Stop()

	for {
		infoRaw, err := redisClient.Info(ctx, "persistence").Result()
		if err != nil {
			return fmt.Errorf("INFO persistence: %w", err)
		}
		info := parseInfoKeyValues(infoRaw)

		inProgress, ok := info[inProgressKey]
		if !ok {
			return fmt.Errorf("INFO persistence missing key %q", inProgressKey)
		}
		if inProgress == "0" {
			if status, ok := info[lastStatusKey]; ok && status != "" && status != "ok" {
				return fmt.Errorf("%s=%s", lastStatusKey, status)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for persistence completion: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseInfoKeyValues(raw string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(raw, "\r\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func archiveDirectoryAsTarGz(directoryPath, requiredFile string) (string, error) {
	requiredPath := filepath.Join(directoryPath, requiredFile)
	if _, err := os.Stat(requiredPath); err != nil {
		return "", fmt.Errorf("required file %s is missing: %w", requiredPath, err)
	}

	tempFile, err := os.CreateTemp("", "redis-backup-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("creating temp archive file: %w", err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("closing temp archive file: %w", err)
	}

	if err := writeTarGz(directoryPath, tempPath); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	return tempPath, nil
}

func writeTarGz(sourceDir, archivePath string) error {
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening archive file %s: %w", archivePath, err)
	}

	gzipWriter := gzip.NewWriter(archiveFile)

	tarWriter := tar.NewWriter(gzipWriter)

	walkErr := filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceDir {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("reading file info for %s: %w", path, err)
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("building tar header for %s: %w", path, err)
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", path, err)
		}

		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type %s (%s)", path, info.Mode().String())
		}

		src, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}

		_, copyErr := io.Copy(tarWriter, src)
		closeErr := src.Close()
		if copyErr != nil {
			return fmt.Errorf("archiving %s: %w", path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing source file %s: %w", path, closeErr)
		}

		return nil
	})

	tarCloseErr := tarWriter.Close()
	gzipCloseErr := gzipWriter.Close()
	fileCloseErr := archiveFile.Close()

	if walkErr != nil {
		closeErr := errors.Join(tarCloseErr, gzipCloseErr, fileCloseErr)
		if closeErr != nil {
			return fmt.Errorf("creating tar.gz archive from %s: %w", sourceDir, errors.Join(walkErr, closeErr))
		}
		return fmt.Errorf("creating tar.gz archive from %s: %w", sourceDir, walkErr)
	}
	if tarCloseErr != nil {
		return fmt.Errorf("closing tar writer: %w", tarCloseErr)
	}
	if gzipCloseErr != nil {
		return fmt.Errorf("closing gzip writer: %w", gzipCloseErr)
	}
	if fileCloseErr != nil {
		return fmt.Errorf("closing archive file %s: %w", archivePath, fileCloseErr)
	}

	return nil
}

func (s *Server) uploadBackupToS3(
	ctx context.Context,
	backupName string,
	destination *redisv1.S3Destination,
	artifactPath string,
	artifactType redisv1.BackupArtifactType,
) (*backupResponse, error) {
	accessKeyID, secretAccessKey, sessionToken, err := readAWSCredentialsFromDir(s.backupCredentialsMountDir())
	if err != nil {
		return nil, err
	}

	s3Client, err := newS3Client(ctx, destination, accessKeyID, secretAccessKey, sessionToken)
	if err != nil {
		return nil, err
	}

	artifactFile, err := os.Open(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("opening artifact %s: %w", artifactPath, err)
	}
	defer func() { _ = artifactFile.Close() }()

	objectKey := buildObjectKey(destination.Path, backupName, artifactType)
	if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(destination.Bucket),
		Key:    aws.String(objectKey),
		Body:   artifactFile,
	}); err != nil {
		return nil, fmt.Errorf("uploading to s3://%s/%s: %w", destination.Bucket, objectKey, err)
	}

	info, err := os.Stat(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("reading artifact metadata for %s: %w", artifactPath, err)
	}

	checksum, err := fileChecksumSHA256(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("computing checksum for %s: %w", artifactPath, err)
	}

	return &backupResponse{
		ArtifactType:   artifactType,
		BackupPath:     fmt.Sprintf("s3://%s/%s", destination.Bucket, objectKey),
		ObjectKey:      objectKey,
		BackupSize:     info.Size(),
		ChecksumSHA256: checksum,
	}, nil
}

func (s *Server) backupDataDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return defaultBackupDataDir
}

func (s *Server) backupCredentialsMountDir() string {
	if s.backupCredentialsDir != "" {
		return s.backupCredentialsDir
	}
	return defaultBackupCredsMountPath
}

func buildObjectKey(pathPrefix, backupName string, artifactType redisv1.BackupArtifactType) string {
	extension := ".rdb"
	if artifactType == redisv1.BackupArtifactTypeAOFArchive {
		extension = ".aof.tar.gz"
	}

	objectName := backupName + extension
	prefix := strings.Trim(pathPrefix, "/")
	if prefix == "" {
		return objectName
	}
	return prefix + "/" + objectName
}

func readAWSCredentialsFromDir(mountDir string) (string, string, string, error) {
	accessKeyID, err := firstCredentialValue(
		mountDir,
		"AWS_ACCESS_KEY_ID",
		"aws_access_key_id",
		"accessKeyId",
		"access_key_id",
	)
	if err != nil {
		return "", "", "", err
	}
	secretAccessKey, err := firstCredentialValue(
		mountDir,
		"AWS_SECRET_ACCESS_KEY",
		"aws_secret_access_key",
		"secretAccessKey",
		"secret_access_key",
	)
	if err != nil {
		return "", "", "", err
	}
	sessionToken, err := firstCredentialValue(
		mountDir,
		"AWS_SESSION_TOKEN",
		"aws_session_token",
		"sessionToken",
		"session_token",
	)
	if err != nil {
		return "", "", "", err
	}

	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", "", fmt.Errorf("backup credentials secret must include AWS access key and secret key")
	}

	return accessKeyID, secretAccessKey, sessionToken, nil
}

func firstCredentialValue(mountDir string, names ...string) (string, error) {
	for _, name := range names {
		path := filepath.Join(mountDir, name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reading credential file %s: %w", path, err)
		}

		value := strings.TrimSpace(string(raw))
		if value != "" {
			return value, nil
		}
	}

	return "", nil
}

func newS3Client(
	ctx context.Context,
	destination *redisv1.S3Destination,
	accessKeyID string,
	secretAccessKey string,
	sessionToken string,
) (*s3.Client, error) {
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
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		if destination.Endpoint != "" {
			options.BaseEndpoint = aws.String(destination.Endpoint)
			options.UsePathStyle = true
		}
	}), nil
}

func fileChecksumSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // read-only file close is best effort

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
