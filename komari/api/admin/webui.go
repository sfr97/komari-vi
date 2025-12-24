package admin

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/api"
	"github.com/komari-monitor/komari/database/config"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/public"
)

const (
	maxWebUIZipBytes          = int64(250 << 20)  // 250 MiB
	maxWebUIUncompressedBytes = uint64(600 << 20) // 600 MiB
	maxWebUIFiles             = 20000
)

type webuiInstallResult struct {
	Files int    `json:"files"`
	Bytes uint64 `json:"bytes"`
	Base  string `json:"base"` // "", "dist/", "<prefix>/dist/"
}

// UploadWebUI 上传并安装 WebUI 覆盖包（komari-web 的构建产物 zip）。
//
// zip 内部支持两种结构：
// 1) 直接包含 index.html / assets...（dist 内容作为根目录）
// 2) 包含 dist/index.html / dist/assets...（自动剥离 dist/）
func UploadWebUI(c *gin.Context) {
	tmpZip, err := saveUploadedZip(c)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	defer os.Remove(tmpZip)

	result, err := installWebUIFromZip(tmpZip)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}

	// 重新加载本地 WebUI（使其立即生效）
	if cfg, err := config.Get(); err == nil {
		public.ReloadLocalWebUI(cfg)
	} else {
		public.ReloadLocalWebUI(models.Config{})
	}

	api.RespondSuccessMessage(c, "WebUI 更新成功", result)
}

func saveUploadedZip(c *gin.Context) (string, error) {
	tmp, err := os.CreateTemp("", "komari-webui-*.zip")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %v", err)
	}
	defer tmp.Close()

	var src io.Reader
	if fh, err := c.FormFile("file"); err == nil && fh != nil {
		f, err := fh.Open()
		if err != nil {
			return "", fmt.Errorf("读取上传文件失败: %v", err)
		}
		defer f.Close()
		src = f
	} else {
		src = c.Request.Body
	}

	n, err := io.Copy(tmp, io.LimitReader(src, maxWebUIZipBytes+1))
	if err != nil {
		return "", fmt.Errorf("保存上传文件失败: %v", err)
	}
	if n == 0 {
		return "", errors.New("请选择要上传的 zip 文件")
	}
	if n > maxWebUIZipBytes {
		return "", fmt.Errorf("zip 文件过大（>%d MiB）", maxWebUIZipBytes>>20)
	}

	return tmp.Name(), nil
}

func installWebUIFromZip(zipPath string) (webuiInstallResult, error) {
	var result webuiInstallResult

	if err := os.MkdirAll("./data", 0755); err != nil {
		return result, fmt.Errorf("创建 data 目录失败: %v", err)
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return result, fmt.Errorf("无法打开ZIP文件: %v", err)
	}
	defer r.Close()

	basePrefix, err := detectWebUIBasePrefix(r.File)
	if err != nil {
		return result, err
	}
	result.Base = basePrefix

	tmpDir, err := os.MkdirTemp("./data", "webui_tmp_")
	if err != nil {
		return result, fmt.Errorf("创建临时目录失败: %v", err)
	}
	// 清理临时目录（若后续 swap 成功会被移动到最终目录，不需要清理）
	defer os.RemoveAll(tmpDir)

	var totalBytes uint64
	var totalFiles int

	for _, f := range r.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if !strings.HasPrefix(name, basePrefix) {
			continue
		}
		rel := strings.TrimPrefix(name, basePrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}

		// 安全检查：拒绝路径遍历
		cleanRel := path.Clean("/" + rel)
		if strings.HasPrefix(cleanRel, "/../") || cleanRel == "/.." {
			return result, fmt.Errorf("zip 包含非法路径: %s", f.Name)
		}
		cleanRel = strings.TrimPrefix(cleanRel, "/")
		if cleanRel == "" || cleanRel == "." {
			continue
		}

		// 拒绝 symlink（避免解压后逃逸/覆盖）
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("zip 包含不支持的符号链接: %s", f.Name)
		}

		// 资源限制（防 zip bomb）
		totalBytes += f.UncompressedSize64
		totalFiles++
		if totalFiles > maxWebUIFiles {
			return result, fmt.Errorf("zip 文件数量过多（>%d）", maxWebUIFiles)
		}
		if totalBytes > maxWebUIUncompressedBytes {
			return result, fmt.Errorf("解压后总体积过大（>%d MiB）", maxWebUIUncompressedBytes>>20)
		}

		dstPath := filepath.Join(tmpDir, filepath.FromSlash(cleanRel))
		if !strings.HasPrefix(dstPath, filepath.Clean(tmpDir)+string(os.PathSeparator)) {
			return result, fmt.Errorf("zip 包含非法路径: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return result, fmt.Errorf("创建目录失败: %v", err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return result, fmt.Errorf("创建目录失败: %v", err)
		}

		rc, err := f.Open()
		if err != nil {
			return result, fmt.Errorf("打开压缩文件失败: %v", err)
		}

		out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			rc.Close()
			return result, fmt.Errorf("创建文件失败: %v", err)
		}

		_, copyErr := io.Copy(out, rc)
		_ = out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return result, fmt.Errorf("解压文件失败: %v", copyErr)
		}
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "index.html")); err != nil {
		return result, errors.New("zip 中未找到 WebUI 的 index.html（请上传 komari-web 构建产物）")
	}

	finalDir := "./data/webui"
	backupDir := "./data/webui.bak"

	_ = os.RemoveAll(backupDir)
	if _, err := os.Stat(finalDir); err == nil {
		if err := os.Rename(finalDir, backupDir); err != nil {
			return result, fmt.Errorf("替换旧 WebUI 失败: %v", err)
		}
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		// 回滚
		if _, stErr := os.Stat(backupDir); stErr == nil {
			_ = os.Rename(backupDir, finalDir)
		}
		return result, fmt.Errorf("安装 WebUI 失败: %v", err)
	}
	_ = os.RemoveAll(backupDir)

	// swap 成功：避免 defer 把已移动目录删掉
	result.Files = totalFiles
	result.Bytes = totalBytes
	return result, nil
}

func detectWebUIBasePrefix(files []*zip.File) (string, error) {
	type candidate struct {
		prefix string
		parts  int
	}

	var best *candidate
	for _, f := range files {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		name = strings.TrimPrefix(name, "/")
		if name == "index.html" {
			c := &candidate{prefix: "", parts: 0}
			if best == nil || c.parts < best.parts {
				best = c
			}
			continue
		}
		if name == "dist/index.html" {
			c := &candidate{prefix: "dist/", parts: 1}
			if best == nil || c.parts < best.parts {
				best = c
			}
			continue
		}
		if strings.HasSuffix(name, "/dist/index.html") {
			p := strings.TrimSuffix(name, "index.html")
			p = strings.TrimSuffix(p, "/")
			prefix := p + "/"
			parts := len(strings.Split(strings.TrimSuffix(prefix, "/"), "/"))
			c := &candidate{prefix: prefix, parts: parts}
			if best == nil || c.parts < best.parts {
				best = c
			}
		}
	}

	if best == nil {
		return "", errors.New("zip 中未找到 index.html 或 dist/index.html（请上传 komari-web 构建产物）")
	}

	// 只支持根目录或 dist 目录结构（以及可选的单层/多层前缀 + dist）
	if best.prefix != "" && !strings.HasSuffix(best.prefix, "dist/") {
		return "", errors.New("zip 结构不受支持（仅支持 dist/ 或 dist 内容作为根目录）")
	}
	return best.prefix, nil
}
