package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Uploader publishes one workflow artifact through the runner's results
// service. That API is not publicly documented, but it is the same one
// @actions/artifact and actions/upload-artifact speak, so its wire format
// cannot move without breaking GitHub's own tooling.
type Uploader struct {
	baseURL string
	token   string
	runID   string
	jobID   string
	client  *http.Client
}

const servicePrefix = "twirp/github.actions.results.api.v1.ArtifactService/"

// resultsScope is the token scope carrying the identifiers an upload has to
// name, in the form Actions.Results:<run id>:<job id>.
const resultsScope = "Actions.Results:"

// New builds an uploader from the runner environment, or reports that the
// environment cannot publish artifacts.
func New(timeout time.Duration) (*Uploader, error) {
	baseURL := os.Getenv("ACTIONS_RESULTS_URL")
	token := os.Getenv("ACTIONS_RUNTIME_TOKEN")
	if baseURL == "" || token == "" {
		return nil, errors.New("the runner artifact service is unavailable in this step")
	}
	runID, jobID, err := backendIDs(token)
	if err != nil {
		return nil, err
	}
	return &Uploader{
		baseURL: strings.TrimRight(baseURL, "/") + "/",
		token:   token,
		runID:   runID,
		jobID:   jobID,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// backendIDs reads the run and job identifiers out of the runtime token. The
// token is only decoded, never verified: it is this process's own credential
// and the service checks it.
func backendIDs(token string) (string, string, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return "", "", errors.New("the runtime token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return "", "", errors.New("the runtime token payload is not base64url")
	}
	var claims struct {
		Scope string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", errors.New("the runtime token payload is not JSON")
	}
	for _, scope := range strings.Fields(claims.Scope) {
		if !strings.HasPrefix(scope, resultsScope) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(scope, resultsScope), ":")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			break
		}
		return parts[0], parts[1], nil
	}
	return "", "", errors.New("the runtime token carries no artifact scope")
}

// Upload publishes content as a single-file artifact. Artifact names must be
// unique within a job.
func (u *Uploader) Upload(ctx context.Context, name, fileName string, content []byte) error {
	archive, err := zipOne(fileName, content)
	if err != nil {
		return err
	}
	uploadURL, err := u.createArtifact(ctx, name)
	if err != nil {
		return err
	}
	if err := u.putBlock(ctx, uploadURL, archive); err != nil {
		return err
	}
	return u.finalizeArtifact(ctx, name, archive)
}

func zipOne(fileName string, content []byte) ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create(fileName)
	if err != nil {
		return nil, fmt.Errorf("create artifact entry: %w", err)
	}
	if _, err := entry.Write(content); err != nil {
		return nil, fmt.Errorf("write artifact entry: %w", err)
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close artifact archive: %w", err)
	}
	return buffer.Bytes(), nil
}

func (u *Uploader) createArtifact(ctx context.Context, name string) (string, error) {
	var response struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signed_upload_url"`
	}
	request := map[string]any{
		"workflow_run_backend_id":     u.runID,
		"workflow_job_run_backend_id": u.jobID,
		"name":                        name,
		"version":                     4,
	}
	if err := u.call(ctx, "CreateArtifact", request, &response); err != nil {
		return "", err
	}
	if !response.OK || response.SignedUploadURL == "" {
		return "", errors.New("the artifact service declined to create the artifact")
	}
	return response.SignedUploadURL, nil
}

func (u *Uploader) finalizeArtifact(ctx context.Context, name string, archive []byte) error {
	sum := sha256.Sum256(archive)
	request := map[string]any{
		"workflow_run_backend_id":     u.runID,
		"workflow_job_run_backend_id": u.jobID,
		"name":                        name,
		"size":                        len(archive),
		"hash":                        "sha256:" + hex.EncodeToString(sum[:]),
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if err := u.call(ctx, "FinalizeArtifact", request, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New("the artifact service declined to finalize the artifact")
	}
	return nil
}

func (u *Uploader) call(ctx context.Context, method string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		u.baseURL+servicePrefix+method,
		bytes.NewReader(encoded),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+u.token)
	response, err := u.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s failed", method)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", method, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (u *Uploader) putBlock(ctx context.Context, uploadURL string, archive []byte) error {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		uploadURL,
		bytes.NewReader(archive),
	)
	if err != nil {
		return err
	}
	request.Header.Set("x-ms-blob-type", "BlockBlob")
	request.Header.Set("Content-Type", "application/zip")
	request.ContentLength = int64(len(archive))
	response, err := u.client.Do(request)
	if err != nil {
		return errors.New("uploading the artifact failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("uploading the artifact returned HTTP %d", response.StatusCode)
	}
	return nil
}
