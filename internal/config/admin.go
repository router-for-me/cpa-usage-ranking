package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cpa-usage-ranking/internal/admin"
)

func loadAdmin(cfg *Config) error {
	if path, set := lookupTrimmed("RANKING_ADMIN_CONFIG_FILE"); set {
		file, err := os.Open(path)
		if err != nil {
			return errors.New("cannot open administrator configuration file")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.IsDir() || (info.Mode().IsRegular() && info.Size() > 8192) {
			return errors.New("administrator configuration must be a small JSON file")
		}
		// 权限过宽时在启动阶段输出告警，不因容器只读挂载或 Secret 挂载权限阻止服务启动。
		if info.Mode().Perm()&0037 != 0 {
			cfg.AdminConfigInsecurePerm = true
		}
		if cfg.WebDir != "" {
			root, _ := filepath.Abs(cfg.WebDir)
			if resolved, err := filepath.EvalSymlinks(root); err == nil {
				root = resolved
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return errors.New("cannot resolve administrator configuration file")
			}
			target, _ = filepath.Abs(target)
			relative, err := filepath.Rel(root, target)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative) {
				return errors.New("administrator configuration must be outside the frontend directory")
			}
		}
		// 统一以 LimitReader 限长读取，兼容容器 Secret 挂载、符号链接与命名管道。
		data, err := io.ReadAll(io.LimitReader(file, 8193))
		if err != nil || len(data) > 8192 {
			return errors.New("administrator configuration must be a small JSON file")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		var owner admin.Owner
		if decoder.Decode(&owner) != nil || decoder.Decode(new(any)) != io.EOF || !admin.ValidCredentials(owner.Username, owner.Password) {
			return errors.New("invalid administrator configuration: use a 3-64 character ASCII username and a 12-72 byte password")
		}
		cfg.AdminOwner = &owner
	}
	if value, ok := lookupTrimmed("RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP"); ok {
		allow, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP must be a boolean")
		}
		cfg.AdminAllowLocalHTTP = allow
	}
	return nil
}
