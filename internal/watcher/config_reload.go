// config_reload.go implements debounced configuration hot reload.
// It detects material changes and reloads clients when the config changes.
package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
)

func (w *Watcher) stopConfigReloadTimer() {
	w.configReloadMu.Lock()
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
		w.configReloadTimer = nil
	}
	w.configReloadMu.Unlock()
}

func (w *Watcher) scheduleConfigReload() {
	w.configReloadMu.Lock()
	defer w.configReloadMu.Unlock()
	if w.stopped.Load() {
		return
	}
	if w.configReloadTimer != nil {
		w.configReloadTimer.Stop()
	}
	w.configReloadTimer = time.AfterFunc(configReloadDebounce, func() {
		w.configReloadMu.Lock()
		w.configReloadTimer = nil
		w.configReloadMu.Unlock()
		w.reloadConfigIfChanged()
	})
}

// ReloadConfigIfChanged runs the same config reload path used by filesystem events.
func (w *Watcher) ReloadConfigIfChanged() {
	if w == nil {
		return
	}
	w.reloadConfigIfChanged()
}

func (w *Watcher) reloadConfigIfChanged() {
	w.reloadConfigWithForce(false)
}

func (w *Watcher) reloadConfig() bool {
	return w.reloadConfigWithForce(true)
}

func (w *Watcher) reloadConfigWithForce(force bool) bool {
	// Timer callbacks and explicit management reloads share one apply order.
	w.reloadMu.Lock()
	defer w.reloadMu.Unlock()
	if w.stopped.Load() {
		return false
	}
	data, errRead := os.ReadFile(w.configPath)
	if errRead != nil {
		log.Errorf("failed to read config file for reload: %v", errRead)
		return false
	}
	if len(data) == 0 {
		log.Debug("ignoring empty config file write event")
		return false
	}
	newConfig, errLoadConfig := config.LoadConfig(w.configPath)
	if errLoadConfig != nil {
		log.Errorf("failed to reload config: %v", errLoadConfig)
		return false
	}

	// Hash the loaded snapshot, not a later file read. LoadConfig may rewrite a
	// plaintext management secret, and callbacks may run while the next edit arrives.
	snapshot, errMarshal := yaml.Marshal(newConfig)
	if errMarshal != nil {
		log.WithError(errMarshal).Error("failed to serialize loaded config")
		return false
	}
	sum := sha256.Sum256(snapshot)
	newHash := hex.EncodeToString(sum[:])
	w.clientsMutex.RLock()
	currentHash := w.lastConfigHash
	w.clientsMutex.RUnlock()
	if !force && currentHash == newHash {
		log.Debug("loaded config unchanged, skipping reload")
		// Comment-only edits still belong in the remote config mirror.
		w.persistConfigAsync()
		return false
	}
	if !w.applyConfigReload(newConfig) {
		return false
	}
	w.clientsMutex.Lock()
	w.lastConfigHash = newHash
	w.clientsMutex.Unlock()
	w.persistConfigAsync()
	return true
}

func (w *Watcher) applyConfigReload(newConfig *config.Config) bool {
	if w.mirroredAuthDir != "" {
		newConfig.AuthDir = w.mirroredAuthDir
	} else {
		if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(newConfig.AuthDir); errResolveAuthDir != nil {
			log.Errorf("failed to resolve auth directory from config: %v", errResolveAuthDir)
			return false
		} else {
			newConfig.AuthDir = resolvedAuthDir
		}
	}

	w.authRescanMu.Lock()
	defer w.authRescanMu.Unlock()
	w.clientsMutex.RLock()
	oldAuthDir := w.authDir
	w.clientsMutex.RUnlock()
	authDirChanged := oldAuthDir != newConfig.AuthDir
	if authDirChanged && w.watching {
		if errSwitch := w.switchAuthDirWatch(oldAuthDir, newConfig.AuthDir); errSwitch != nil {
			log.WithError(errSwitch).Error("failed to switch auth directory; keeping current config")
			return false
		}
	}

	w.clientsMutex.Lock()
	var oldConfig *config.Config
	_ = yaml.Unmarshal(w.oldConfigYaml, &oldConfig)
	w.oldConfigYaml, _ = yaml.Marshal(newConfig)
	w.config = newConfig
	w.authDir = newConfig.AuthDir
	if authDirChanged {
		w.lastRemoveTimes = nil
	}
	w.clientsMutex.Unlock()

	var affectedOAuthProviders []string
	if oldConfig != nil {
		_, affectedOAuthProviders = diff.DiffOAuthExcludedModelChanges(oldConfig.OAuthExcludedModels, newConfig.OAuthExcludedModels)
	}

	util.SetLogLevel(newConfig)
	if oldConfig != nil && oldConfig.Debug != newConfig.Debug {
		log.Debugf("log level updated - debug mode changed from %t to %t", oldConfig.Debug, newConfig.Debug)
	}

	if oldConfig != nil {
		details := diff.BuildConfigChangeDetails(oldConfig, newConfig)
		if len(details) > 0 {
			log.Info("config changes detected:")
			for _, d := range details {
				log.Infof("  %s", d)
			}
		} else {
			log.Debugf("no material config field changes detected")
		}
	}

	retryConfigChanged := oldConfig != nil && (oldConfig.RequestRetry != newConfig.RequestRetry || oldConfig.MaxRetryInterval != newConfig.MaxRetryInterval || oldConfig.MaxRetryCredentials != newConfig.MaxRetryCredentials)
	forceAuthRefresh := oldConfig != nil && (oldConfig.ForceModelPrefix != newConfig.ForceModelPrefix || !reflect.DeepEqual(oldConfig.OAuthModelAlias, newConfig.OAuthModelAlias) || retryConfigChanged)

	log.Infof("config successfully reloaded, triggering client reload")
	w.reloadClientsLocked(authDirChanged || oldConfig == nil, affectedOAuthProviders, forceAuthRefresh)
	return true
}

// switchAuthDirWatch adds the new watch before retiring the old one. The caller
// holds authRescanMu and publishes the new directory only after this succeeds.
func (w *Watcher) switchAuthDirWatch(oldDir, newDir string) error {
	info, errStat := os.Stat(newDir)
	if errStat != nil {
		return errStat
	}
	if !info.IsDir() {
		return fmt.Errorf("auth path is not a directory: %s", newDir)
	}
	if errAdd := w.watcher.Add(newDir); errAdd != nil {
		return errAdd
	}
	// The config directory may share the auth watch and must remain watched.
	if w.sameWatchPath(oldDir, newDir) || w.sameWatchPath(oldDir, filepath.Dir(w.configPath)) {
		return nil
	}
	if errRemove := w.watcher.Remove(oldDir); errRemove != nil && !errors.Is(errRemove, fsnotify.ErrNonExistentWatch) {
		if !w.sameWatchPath(newDir, filepath.Dir(w.configPath)) {
			if errRollback := w.watcher.Remove(newDir); errRollback != nil {
				return errors.Join(errRemove, errRollback)
			}
		}
		return errRemove
	}
	return nil
}

func (w *Watcher) sameWatchPath(left, right string) bool {
	leftAbs, errLeft := filepath.Abs(left)
	rightAbs, errRight := filepath.Abs(right)
	if errLeft != nil || errRight != nil {
		return w.normalizeAuthPath(left) == w.normalizeAuthPath(right)
	}
	return w.normalizeAuthPath(leftAbs) == w.normalizeAuthPath(rightAbs)
}
