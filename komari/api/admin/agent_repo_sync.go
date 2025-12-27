package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/api"
	"github.com/komari-monitor/komari/database/agentversion"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

type githubRelease struct {
	ID          int64         `json:"id"`
	Name        string        `json:"name"`
	TagName     string        `json:"tag_name"`
	Body        string        `json:"body"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt *time.Time    `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
}

type repoSyncPreviewRequest struct {
	Repo              string `json:"repo"`
	Keyword           string `json:"keyword"`
	IncludePrerelease bool   `json:"include_prerelease"`
}

type repoSyncPreviewRelease struct {
	ReleaseID   int64                  `json:"release_id"`
	Name        string                 `json:"name"`
	TagName     string                 `json:"tag_name"`
	Body        string                 `json:"body"`
	Prerelease  bool                   `json:"prerelease"`
	PublishedAt *time.Time             `json:"published_at"`
	Assets      []repoSyncPreviewAsset `json:"assets"`
}

type repoSyncPreviewAsset struct {
	AssetID     int64  `json:"asset_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
	ContentType string `json:"content_type"`
	IsValid     bool   `json:"is_valid"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
}

type repoSyncStartRequest struct {
	Repo       string  `json:"repo"`
	ReleaseID  int64   `json:"release_id"`
	AssetIDs   []int64 `json:"asset_ids"`
	SetCurrent bool    `json:"set_current"`
	Proxy      string  `json:"proxy"`
}

type repoSyncStartResponse struct {
	SessionID string `json:"session_id"`
}

type repoSyncProgress struct {
	CurrentFile       string `json:"current_file"`
	FileDownloaded    int64  `json:"file_downloaded"`
	FileTotal         int64  `json:"file_total"`
	OverallDownloaded int64  `json:"overall_downloaded"`
	OverallTotal      int64  `json:"overall_total"`
	Index             int    `json:"index"`
	Count             int    `json:"count"`
}

type sseEvent struct {
	Event string
	Data  string
}

type repoSyncFileState struct {
	AssetID    int64  `json:"asset_id"`
	Name       string `json:"name"`
	State      string `json:"state"` // pending/downloading/done/error
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Error      string `json:"error,omitempty"`
}

type repoSyncSession struct {
	ID        string
	CreatedAt time.Time
	Done      bool
	Err       string

	mu       sync.Mutex
	logs     []string
	progress repoSyncProgress
	files    map[int64]repoSyncFileState
	subs     map[chan sseEvent]struct{}
}

var (
	repoSyncSessionsMu sync.Mutex
	repoSyncSessions   = map[string]*repoSyncSession{}
)

func cleanupOldRepoSyncSessionsLocked(now time.Time) {
	for id, s := range repoSyncSessions {
		s.mu.Lock()
		done := s.Done
		createdAt := s.CreatedAt
		s.mu.Unlock()
		if done && now.Sub(createdAt) > 6*time.Hour {
			delete(repoSyncSessions, id)
		}
	}
}

func newRepoSyncSession() *repoSyncSession {
	s := &repoSyncSession{
		ID:        uuid.NewString(),
		CreatedAt: time.Now(),
		subs:      map[chan sseEvent]struct{}{},
		logs:      make([]string, 0, 200),
		files:     map[int64]repoSyncFileState{},
	}
	repoSyncSessionsMu.Lock()
	cleanupOldRepoSyncSessionsLocked(s.CreatedAt)
	repoSyncSessions[s.ID] = s
	repoSyncSessionsMu.Unlock()
	return s
}

func getRepoSyncSession(id string) (*repoSyncSession, bool) {
	repoSyncSessionsMu.Lock()
	defer repoSyncSessionsMu.Unlock()
	s, ok := repoSyncSessions[id]
	return s, ok
}

func (s *repoSyncSession) appendLog(line string) {
	line = strings.TrimRight(line, "\r\n")
	s.mu.Lock()
	if len(s.logs) >= 2000 {
		s.logs = s.logs[len(s.logs)-1000:]
	}
	s.logs = append(s.logs, line)
	ev := sseEvent{Event: "log", Data: line}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *repoSyncSession) emit(event string, payload any) {
	raw, _ := json.Marshal(payload)
	s.mu.Lock()
	ev := sseEvent{Event: event, Data: string(raw)}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *repoSyncSession) setProgress(p repoSyncProgress) {
	s.mu.Lock()
	s.progress = p
	raw, _ := json.Marshal(p)
	ev := sseEvent{Event: "progress", Data: string(raw)}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *repoSyncSession) initFiles(assets []githubAsset) {
	s.mu.Lock()
	if s.files == nil {
		s.files = map[int64]repoSyncFileState{}
	}
	for _, a := range assets {
		s.files[a.ID] = repoSyncFileState{
			AssetID: a.ID,
			Name:    a.Name,
			State:   "pending",
			Total:   a.Size,
		}
	}
	s.mu.Unlock()
}

func (s *repoSyncSession) setFileProgress(assetID int64, downloaded, total int64) {
	s.mu.Lock()
	st, ok := s.files[assetID]
	if ok {
		st.Downloaded = downloaded
		if total > 0 {
			st.Total = total
		}
		s.files[assetID] = st
	}
	s.mu.Unlock()
}

func (s *repoSyncSession) setFileState(assetID int64, state string, errMsg string) repoSyncFileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.files[assetID]
	if !ok {
		st = repoSyncFileState{AssetID: assetID}
	}
	st.State = state
	st.Error = strings.TrimSpace(errMsg)
	s.files[assetID] = st
	return st
}

func (s *repoSyncSession) snapshotFiles() []repoSyncFileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]repoSyncFileState, 0, len(s.files))
	for _, st := range s.files {
		out = append(out, st)
	}
	return out
}

func (s *repoSyncSession) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Done = true
	if err != nil {
		s.Err = err.Error()
	}
	for ch := range s.subs {
		close(ch)
	}
	s.subs = map[chan sseEvent]struct{}{}
}

func (s *repoSyncSession) snapshot() (logs []string, progress repoSyncProgress, done bool, errStr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.logs))
	copy(cp, s.logs)
	return cp, s.progress, s.Done, s.Err
}

func (s *repoSyncSession) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 200)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normalizeRepoInput(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", fmt.Errorf("仓库地址不能为空")
	}
	// URL 形式
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("仓库地址格式错误")
		}
		path := strings.Trim(u.Path, "/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			return "", fmt.Errorf("仓库地址缺少 owner/repo")
		}
		owner := parts[0]
		repo := strings.TrimSuffix(parts[1], ".git")
		normalized := owner + "/" + repo
		if !repoPattern.MatchString(normalized) {
			return "", fmt.Errorf("仓库地址不合法")
		}
		return normalized, nil
	}

	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "www.github.com/")
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("仓库地址缺少 owner/repo")
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	normalized := owner + "/" + repo
	if !repoPattern.MatchString(normalized) {
		return "", fmt.Errorf("仓库地址不合法")
	}
	return normalized, nil
}

func githubAPIClient() *http.Client {
	return &http.Client{Timeout: 25 * time.Second}
}

func githubDownloadClient() *http.Client {
	// 下载超时交给请求 Context 控制
	return &http.Client{}
}

func fetchReleases(ctx context.Context, repo string) ([]githubRelease, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "komari")
	resp, err := githubAPIClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("GitHub API 请求失败: %s", msg)
	}
	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}
	return releases, nil
}

func fetchReleaseByID(ctx context.Context, repo string, id int64) (*githubRelease, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/%d", repo, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "komari")
	resp, err := githubAPIClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("GitHub API 请求失败: %s", msg)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func containsInsensitive(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func joinProxy(proxy, rawURL string) (string, error) {
	p := strings.TrimSpace(proxy)
	if p == "" {
		return rawURL, nil
	}
	if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
		p = "https://" + p
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("代理地址不合法")
	}
	return strings.TrimRight(p, "/") + "/" + rawURL, nil
}

func PreviewRepoSync(c *gin.Context) {
	var req repoSyncPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	repoInput := strings.TrimSpace(req.Repo)
	if repoInput == "" {
		repoInput = "danger-dream/komari-vi"
	}
	repo, err := normalizeRepoInput(repoInput)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	keyword := strings.TrimSpace(req.Keyword)
	if keyword == "" {
		keyword = "agent"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	releases, err := fetchReleases(ctx, repo)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}

	out := make([]repoSyncPreviewRelease, 0, len(releases))
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if !req.IncludePrerelease && r.Prerelease {
			continue
		}
		if !containsInsensitive(r.Name, keyword) && !containsInsensitive(r.TagName, keyword) {
			continue
		}
		assets := make([]repoSyncPreviewAsset, 0, len(r.Assets))
		for _, a := range r.Assets {
			osName, arch, perr := parsePackageName(a.Name)
			it := repoSyncPreviewAsset{
				AssetID:     a.ID,
				Name:        a.Name,
				Size:        a.Size,
				DownloadURL: a.BrowserDownloadURL,
				ContentType: a.ContentType,
				IsValid:     perr == nil,
			}
			if perr == nil {
				it.OS = osName
				it.Arch = arch
			}
			assets = append(assets, it)
		}
		out = append(out, repoSyncPreviewRelease{
			ReleaseID:   r.ID,
			Name:        r.Name,
			TagName:     r.TagName,
			Body:        r.Body,
			Prerelease:  r.Prerelease,
			PublishedAt: r.PublishedAt,
			Assets:      assets,
		})
	}
	api.RespondSuccess(c, gin.H{
		"repo":     repo,
		"keyword":  keyword,
		"releases": out,
	})
}

func StartRepoSync(c *gin.Context) {
	var req repoSyncStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	repoInput := strings.TrimSpace(req.Repo)
	if repoInput == "" {
		repoInput = "danger-dream/komari-vi"
	}
	repo, err := normalizeRepoInput(repoInput)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.ReleaseID <= 0 {
		api.RespondError(c, http.StatusBadRequest, "请选择要同步的版本")
		return
	}
	if len(req.AssetIDs) == 0 {
		api.RespondError(c, http.StatusBadRequest, "请选择要同步的包")
		return
	}

	s := newRepoSyncSession()
	api.RespondSuccess(c, repoSyncStartResponse{SessionID: s.ID})

	go func() {
		s.appendLog("[INFO] Fetching release info...")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		rel, err := fetchReleaseByID(ctx, repo, req.ReleaseID)
		if err != nil {
			s.finish(err)
			return
		}
		if rel.Draft {
			s.finish(fmt.Errorf("不能同步 draft 版本"))
			return
		}
		if strings.TrimSpace(rel.TagName) == "" {
			s.finish(fmt.Errorf("release 缺少 tag_name"))
			return
		}

		versionName := strings.TrimSpace(rel.TagName)
		if err := agentversion.ValidateVersionName(versionName); err != nil {
			s.finish(err)
			return
		}

		if err := agentversion.EnsurePackageDir(); err != nil {
			s.finish(fmt.Errorf("创建存储目录失败: %w", err))
			return
		}
		versionDir := agentversion.VersionDir(versionName)
		if err := os.MkdirAll(versionDir, os.ModePerm); err != nil {
			s.finish(fmt.Errorf("创建版本目录失败: %w", err))
			return
		}

		v, vErr := agentversion.GetVersionByName(versionName)
		if vErr != nil {
			if errors.Is(vErr, gorm.ErrRecordNotFound) {
				created, cErr := agentversion.CreateVersion(versionName, rel.Body, false, nil)
				if cErr != nil {
					// 并发创建时回退读取
					v, vErr = agentversion.GetVersionByName(versionName)
					if vErr != nil {
						s.finish(cErr)
						return
					}
				} else {
					v = created
				}
			} else {
				s.finish(vErr)
				return
			}
		} else {
			// 同步时更新 changelog（以 GitHub release body 为准）
			body := rel.Body
			_, _ = agentversion.UpdateMetadata(v.ID, nil, &body, nil)
		}

		assetSet := map[int64]struct{}{}
		for _, id := range req.AssetIDs {
			if id > 0 {
				assetSet[id] = struct{}{}
			}
		}
		selected := make([]githubAsset, 0, len(assetSet))
		for _, a := range rel.Assets {
			if _, ok := assetSet[a.ID]; ok {
				selected = append(selected, a)
			}
		}
		if len(selected) == 0 {
			s.finish(fmt.Errorf("未找到选中的资源"))
			return
		}
		s.initFiles(selected)

		var overallTotal int64
		for _, a := range selected {
			if a.Size > 0 {
				overallTotal += a.Size
			}
		}

		var overallDownloaded int64
		for i, a := range selected {
			s.appendLog(fmt.Sprintf("[INFO] Downloading %s (%s bytes)...", a.Name, strconv.FormatInt(a.Size, 10)))
			s.emit("file", s.setFileState(a.ID, "downloading", ""))

			downloadURL := a.BrowserDownloadURL
			if downloadURL == "" {
				s.emit("file", s.setFileState(a.ID, "error", "资源缺少下载地址"))
				s.finish(fmt.Errorf("资源缺少下载地址: %s", a.Name))
				return
			}
			finalURL, err := joinProxy(req.Proxy, downloadURL)
			if err != nil {
				s.emit("file", s.setFileState(a.ID, "error", err.Error()))
				s.finish(err)
				return
			}

			osName, arch, err := parsePackageName(a.Name)
			if err != nil {
				s.emit("file", s.setFileState(a.ID, "error", err.Error()))
				s.finish(err)
				return
			}

			old, _ := agentversion.GetPackageByPlatform(v.ID, osName, arch)

			tmp, err := os.CreateTemp(versionDir, ".repo-sync-*")
			if err != nil {
				s.finish(err)
				return
			}
			tmpPath := tmp.Name()
			tmpClosed := false
			cleanupTmp := func() {
				if !tmpClosed {
					_ = tmp.Close()
					tmpClosed = true
				}
				if tmpPath != "" {
					_ = os.Remove(tmpPath)
					tmpPath = ""
				}
			}

			func() {
				reqCtx, reqCancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer reqCancel()
				httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, finalURL, nil)
				if err != nil {
					cleanupTmp()
					s.emit("file", s.setFileState(a.ID, "error", err.Error()))
					s.finish(err)
					return
				}
				httpReq.Header.Set("User-Agent", "komari")
				resp, err := githubDownloadClient().Do(httpReq)
				if err != nil {
					cleanupTmp()
					s.emit("file", s.setFileState(a.ID, "error", err.Error()))
					s.finish(err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
					msg := strings.TrimSpace(string(body))
					if msg == "" {
						msg = resp.Status
					}
					cleanupTmp()
					err = fmt.Errorf("下载失败 (%s): %s", a.Name, msg)
					s.emit("file", s.setFileState(a.ID, "error", err.Error()))
					s.finish(err)
					return
				}

				total := resp.ContentLength
				if total <= 0 {
					total = a.Size
				}
				s.setFileProgress(a.ID, 0, total)
				hasher := sha256.New()
				buf := make([]byte, 128*1024)
				var downloaded int64
				lastPush := time.Now()

				for {
					n, rErr := resp.Body.Read(buf)
					if n > 0 {
						if _, wErr := tmp.Write(buf[:n]); wErr != nil {
							cleanupTmp()
							s.emit("file", s.setFileState(a.ID, "error", wErr.Error()))
							s.finish(wErr)
							return
						}
						if _, hErr := hasher.Write(buf[:n]); hErr != nil {
							cleanupTmp()
							s.emit("file", s.setFileState(a.ID, "error", hErr.Error()))
							s.finish(hErr)
							return
						}
						downloaded += int64(n)
						now := time.Now()
						if now.Sub(lastPush) > 120*time.Millisecond {
							s.setFileProgress(a.ID, downloaded, total)
							s.setProgress(repoSyncProgress{
								CurrentFile:       a.Name,
								FileDownloaded:    downloaded,
								FileTotal:         total,
								OverallDownloaded: overallDownloaded + downloaded,
								OverallTotal:      overallTotal,
								Index:             i + 1,
								Count:             len(selected),
							})
							lastPush = now
						}
					}
					if rErr != nil {
						if errors.Is(rErr, io.EOF) {
							break
						}
						cleanupTmp()
						s.emit("file", s.setFileState(a.ID, "error", rErr.Error()))
						s.finish(rErr)
						return
					}
				}

				hash := fmt.Sprintf("%x", hasher.Sum(nil))
				finalName := filepath.Base(a.Name)
				dst := filepath.Join(versionDir, finalName)
				if err := tmp.Close(); err != nil {
					cleanupTmp()
					s.emit("file", s.setFileState(a.ID, "error", err.Error()))
					s.finish(err)
					return
				}
				tmpClosed = true
				if err := os.Rename(tmpPath, dst); err != nil {
					// Windows 下 rename 可能因目标存在失败
					_ = os.Remove(dst)
					if err2 := os.Rename(tmpPath, dst); err2 != nil {
						cleanupTmp()
						s.emit("file", s.setFileState(a.ID, "error", err2.Error()))
						s.finish(err2)
						return
					}
				}

				// rename 成功后，tmpPath 已无效
				tmpPath = ""

				if err := agentversion.UpsertPackage(models.AgentPackage{
					VersionID: v.ID,
					OS:        osName,
					Arch:      arch,
					FileName:  finalName,
					FileSize:  downloaded,
					Hash:      hash,
				}); err != nil {
					// 已落盘的文件保留，方便用户手动处理
					s.emit("file", s.setFileState(a.ID, "error", err.Error()))
					s.finish(err)
					return
				}
				if old != nil && old.FileName != finalName {
					_ = os.Remove(filepath.Join(versionDir, old.FileName))
				}

				overallDownloaded += downloaded
				s.setFileProgress(a.ID, downloaded, total)
				s.emit("file", s.setFileState(a.ID, "done", ""))
				s.setProgress(repoSyncProgress{
					CurrentFile:       a.Name,
					FileDownloaded:    downloaded,
					FileTotal:         total,
					OverallDownloaded: overallDownloaded,
					OverallTotal:      overallTotal,
					Index:             i + 1,
					Count:             len(selected),
				})
			}()

			// 如果上面的匿名函数里发生 finish，会关闭 subs，这里尽量避免继续执行
			_, _, done, _ := s.snapshot()
			if done {
				if tmpPath != "" {
					_ = os.Remove(tmpPath)
				}
				return
			}
		}

		if req.SetCurrent {
			s.appendLog("[INFO] Setting as current version...")
			val := true
			if _, err := agentversion.UpdateMetadata(v.ID, nil, nil, &val); err != nil {
				s.finish(err)
				return
			}
		}

		s.appendLog("[SUCCESS] Repo sync finished")
		s.finish(nil)
	}()
}

func StreamRepoSync(c *gin.Context) {
	id := c.Param("id")
	s, ok := getRepoSyncSession(id)
	if !ok {
		api.RespondError(c, http.StatusNotFound, "session not found")
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	logs, progress, done, errStr := s.snapshot()
	for _, line := range logs {
		fmt.Fprintf(c.Writer, "event: log\ndata: %s\n\n", sseEscape(line))
	}
	for _, st := range s.snapshotFiles() {
		raw, _ := json.Marshal(st)
		fmt.Fprintf(c.Writer, "event: file\ndata: %s\n\n", sseEscape(string(raw)))
	}
	if progress.OverallTotal > 0 || progress.CurrentFile != "" {
		raw, _ := json.Marshal(progress)
		fmt.Fprintf(c.Writer, "event: progress\ndata: %s\n\n", sseEscape(string(raw)))
	}
	c.Writer.Flush()
	if done {
		raw, _ := json.Marshal(gin.H{"success": errStr == "", "error": errStr})
		fmt.Fprintf(c.Writer, "event: done\ndata: %s\n\n", sseEscape(string(raw)))
		c.Writer.Flush()
		return
	}

	ch := s.subscribe()
	notify := c.Request.Context().Done()
	for {
		select {
		case <-notify:
			return
		case ev, ok := <-ch:
			if !ok {
				_, _, done, errStr := s.snapshot()
				if done {
					raw, _ := json.Marshal(gin.H{"success": errStr == "", "error": errStr})
					fmt.Fprintf(c.Writer, "event: done\ndata: %s\n\n", sseEscape(string(raw)))
					c.Writer.Flush()
				}
				return
			}
			fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", ev.Event, sseEscape(ev.Data))
			c.Writer.Flush()
		}
	}
}
