package pluginhost

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func waitForHostLoadCleanup(t *testing.T, h *Host, id string) {
	t.Helper()
	h.mu.Lock()
	request := h.loading[id]
	h.mu.Unlock()
	if request != nil {
		waitForHostTestSignal(t, request.done, "plugin load cleanup")
	}
}

func TestHostCanceledReplacementReleasesApplyLockAndKeepsPluginFence(t *testing.T) {
	for _, blockedStage := range []string{"register", "shutdown", "rollback", "quiesce"} {
		t.Run(blockedStage, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			block := func() {
				close(blocked)
				<-release
			}
			var oldReconfigures atomic.Int32
			var thinkingIdentifiers atomic.Int32
			oldPlugin := validTestPlugin("alpha")
			oldPlugin.Capabilities.ThinkingApplier = testThinkingCapability{provider: "fenced-thinking"}
			var oldQuiescing atomic.Bool
			var replacementRegistering atomic.Bool
			var prematureShutdown atomic.Bool
			var prematureRollback atomic.Bool
			var replacement *lifecycleTestClient
			old := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodPluginRegister:
					return lifecycleRegistrationResult(oldPlugin)
				case pluginabi.MethodThinkingIdentifier:
					thinkingIdentifiers.Add(1)
					return marshalRPCResult(rpcIdentifierResponse{Identifier: "fenced-thinking"})
				case pluginabi.MethodPluginQuiesce:
					oldQuiescing.Store(true)
					defer oldQuiescing.Store(false)
					if blockedStage == "quiesce" {
						block()
					}
					return marshalRPCResult(rpcEmptyResponse{})
				case pluginabi.MethodPluginReconfigure:
					oldReconfigures.Add(1)
					if oldQuiescing.Load() || replacementRegistering.Load() || (blockedStage != "quiesce" && replacement.shutdownCalls.Load() == 0) {
						prematureRollback.Store(true)
					}
					if blockedStage == "rollback" {
						block()
					}
					return lifecycleRegistrationResult(oldPlugin)
				default:
					return nil, fmt.Errorf("unexpected old plugin method %s", method)
				}
			}}
			replacement = &lifecycleTestClient{
				call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
					if method != pluginabi.MethodPluginRegister {
						return nil, fmt.Errorf("unexpected replacement method %s", method)
					}
					replacementRegistering.Store(true)
					defer replacementRegistering.Store(false)
					if blockedStage == "register" {
						block()
					}
					return nil, fmt.Errorf("synthetic failed registration")
				},
				shutdown: func() {
					if replacementRegistering.Load() {
						prematureShutdown.Store(true)
					}
					if blockedStage == "shutdown" {
						block()
					}
				},
			}
			beta := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
					return lifecycleRegistrationResult(validTestPlugin("beta"))
				}
				return nil, fmt.Errorf("unexpected beta method %s", method)
			}}
			clients := []pluginClient{old, replacement, beta}
			if blockedStage == "quiesce" {
				clients = []pluginClient{old, beta}
			}
			loader := &sequencePluginLoader{clients: clients}
			h := NewForTest(loader)
			t.Cleanup(func() {
				unblock()
				waitForHostLoadCleanup(t, h, "alpha")
				h.ShutdownAll()
			})
			dir, paths := makeVersionedPluginDir(t, "alpha", "1.0.0")
			h.ApplyConfig(context.Background(), versionedPluginHostConfig(t, dir, "1.0.0"))
			writeVersionedPluginFile(t, dir, "alpha", "2.0.0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := versionedPluginHostConfig(t, dir, "2.0.0")
			applyDone := make(chan struct{})
			go func() {
				h.ApplyConfig(ctx, cfg)
				close(applyDone)
			}()
			waitForHostTestSignal(t, blocked, "blocked replacement lifecycle")
			cancel()
			waitForHostTestSignal(t, applyDone, "canceled replacement")
			if !h.PluginBusy("alpha") {
				t.Fatal("cancellation dropped the plugin loading fence")
			}
			if blockedStage == "register" && replacement.shutdownCalls.Load() != 0 {
				t.Fatal("replacement was unloaded while native registration was executing")
			}

			// Both the old path and the replacement path must honor the same fence.
			for _, version := range []string{"1.0.0", "2.0.0"} {
				fencedConfig := versionedPluginHostConfig(t, dir, version)
				fencedDone := make(chan struct{})
				go func() { h.ApplyConfig(context.Background(), fencedConfig); close(fencedDone) }()
				waitForHostTestSignal(t, fencedDone, "fenced same-plugin apply")
			}
			wantReconfigures := int32(0)
			if blockedStage == "rollback" {
				wantReconfigures = 1
			}
			if got := oldReconfigures.Load(); got != wantReconfigures {
				t.Fatalf("same-plugin apply crossed the loading fence: reconfigure calls = %d", got)
			}

			writeVersionedPluginFile(t, dir, "beta", "1.0.0")
			withBeta := versionedPluginHostConfig(t, dir, "2.0.0")
			withBeta.Plugins.Configs["beta"] = enabledPluginConfigWithStoreVersion(t, "1.0.0")
			betaApplied := make(chan struct{})
			go func() { h.ApplyConfig(context.Background(), withBeta); close(betaApplied) }()
			waitForHostTestSignal(t, betaApplied, "unrelated plugin apply")
			if !h.PluginLoaded("beta") || !h.pluginIdentityCurrent("alpha", paths["1.0.0"], "1.0.0") {
				t.Fatal("unrelated apply failed or lost the old plugin snapshot")
			}
			betaUnloaded := make(chan bool, 1)
			go func() { betaUnloaded <- h.UnloadPlugin("beta") }()
			if !waitForHostTestBool(t, betaUnloaded, "unrelated plugin unload") {
				t.Fatal("unrelated plugin did not unload")
			}
			loader.mu.Lock()
			openCalls := loader.calls
			loader.mu.Unlock()
			if openCalls != len(clients) {
				t.Fatalf("Open calls = %d, want %d", openCalls, len(clients))
			}
			if got := thinkingIdentifiers.Load(); got != 1 {
				t.Fatalf("fenced plugin received native identifier calls: %d, want initial call only", got)
			}
			unblock()
			waitForHostLoadCleanup(t, h, "alpha")
			if got := oldReconfigures.Load(); got != 1 {
				t.Fatalf("rollback calls = %d, want 1", got)
			}
			if prematureShutdown.Load() || prematureRollback.Load() {
				t.Fatal("cleanup/rollback ran before the preceding native lifecycle call completed")
			}
			if blockedStage != "quiesce" && replacement.shutdownCalls.Load() != 1 {
				t.Fatal("replacement did not shut down exactly once")
			}
		})
	}
}

func TestHostCanceledReplacementDoesNotRollBackDetachedPlugin(t *testing.T) {
	for _, action := range []string{"unload", "shutdown", "disable"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var rollbacks atomic.Int32
			old := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodPluginRegister:
					return lifecycleRegistrationResult(validTestPlugin("alpha"))
				case pluginabi.MethodPluginReconfigure:
					rollbacks.Add(1)
					return lifecycleRegistrationResult(validTestPlugin("alpha"))
				case pluginabi.MethodPluginQuiesce:
					return marshalRPCResult(rpcEmptyResponse{})
				default:
					return nil, fmt.Errorf("unexpected method %s", method)
				}
			}}
			replacement := &lifecycleTestClient{call: func(_ context.Context, _ string, _ []byte) ([]byte, error) {
				close(started)
				<-release
				return lifecycleRegistrationResult(validTestPlugin("alpha"))
			}}
			h := NewForTest(&sequencePluginLoader{clients: []pluginClient{old, replacement}})
			t.Cleanup(func() { unblock(); waitForHostLoadCleanup(t, h, "alpha"); h.ShutdownAll() })
			dir, _ := makeVersionedPluginDir(t, "alpha", "1.0.0")
			h.ApplyConfig(context.Background(), versionedPluginHostConfig(t, dir, "1.0.0"))
			writeVersionedPluginFile(t, dir, "alpha", "2.0.0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := versionedPluginHostConfig(t, dir, "2.0.0")
			applyDone := make(chan struct{})
			go func() { h.ApplyConfig(ctx, cfg); close(applyDone) }()
			waitForHostTestSignal(t, started, "replacement registration")
			cancel()
			waitForHostTestSignal(t, applyDone, "canceled replacement apply")
			detached := make(chan struct{})
			go func() {
				switch action {
				case "unload":
					h.UnloadPlugin("alpha")
				case "shutdown":
					h.ShutdownAll()
				case "disable":
					h.ApplyConfig(context.Background(), &config.Config{})
				}
				close(detached)
			}()
			waitForHostTestSignal(t, detached, "detach old plugin during replacement cleanup")
			if !h.PluginBusy("alpha") {
				t.Fatal("detaching the old plugin lost the pending native replacement fence")
			}
			unblock()
			waitForHostLoadCleanup(t, h, "alpha")
			if rollbacks.Load() != 0 || h.PluginRegistered("alpha") {
				t.Fatal("asynchronous rollback resurrected a detached plugin")
			}
			if action != "disable" && h.PluginBusy("alpha") {
				t.Fatal("detached plugin remains busy after physical cleanup")
			}
			if replacement.shutdownCalls.Load() != 1 {
				t.Fatal("replacement did not shut down exactly once")
			}
		})
	}
}

func TestHostCanceledReplacementReconcilesLatestSnapshot(t *testing.T) {
	for _, latestIntent := range []string{"reenable", "disable all", "disable plugin", "unload plugin", "shutdown", "unload unrelated"} {
		t.Run(latestIntent, func(t *testing.T) {
			registerStarted := make(chan struct{})
			releaseRegister := make(chan struct{})
			rollbackStarted := make(chan struct{})
			releaseRollback := make(chan struct{})
			var registerOnce, rollbackOnce sync.Once
			unblockRegister := func() { registerOnce.Do(func() { close(releaseRegister) }) }
			unblockRollback := func() { rollbackOnce.Do(func() { close(releaseRollback) }) }
			var rollbacks atomic.Int32
			oldPlugin := validTestPlugin("alpha")
			oldPlugin.Capabilities.ThinkingApplier = testThinkingCapability{provider: "rollback-thinking"}
			old := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodPluginRegister:
					return lifecycleRegistrationResult(oldPlugin)
				case pluginabi.MethodPluginReconfigure:
					rollbacks.Add(1)
					close(rollbackStarted)
					<-releaseRollback
					return lifecycleRegistrationResult(oldPlugin)
				case pluginabi.MethodThinkingIdentifier:
					return marshalRPCResult(rpcIdentifierResponse{Identifier: "rollback-thinking"})
				case pluginabi.MethodPluginQuiesce:
					return marshalRPCResult(rpcEmptyResponse{})
				default:
					return nil, fmt.Errorf("unexpected old method %s", method)
				}
			}}
			replacement := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				if method != pluginabi.MethodPluginRegister {
					return nil, fmt.Errorf("unexpected replacement method %s", method)
				}
				close(registerStarted)
				<-releaseRegister
				return lifecycleRegistrationResult(validTestPlugin("alpha"))
			}}
			betaPlugin := validTestPlugin("beta")
			betaPlugin.Capabilities.ThinkingApplier = testThinkingCapability{provider: "rollback-thinking"}
			beta := &lifecycleTestClient{call: func(_ context.Context, method string, _ []byte) ([]byte, error) {
				if method == pluginabi.MethodThinkingIdentifier {
					return marshalRPCResult(rpcIdentifierResponse{Identifier: "rollback-thinking"})
				}
				if method != pluginabi.MethodPluginRegister && method != pluginabi.MethodPluginReconfigure {
					return nil, fmt.Errorf("unexpected beta method %s", method)
				}
				return lifecycleRegistrationResult(betaPlugin)
			}}
			h := NewForTest(&sequencePluginLoader{clients: []pluginClient{old, replacement, beta}})
			t.Cleanup(func() {
				unblockRegister()
				unblockRollback()
				waitForHostLoadCleanup(t, h, "alpha")
				h.ShutdownAll()
			})
			dir, paths := makeVersionedPluginDir(t, "alpha", "1.0.0")
			h.ApplyConfig(context.Background(), versionedPluginHostConfig(t, dir, "1.0.0"))
			writeVersionedPluginFile(t, dir, "alpha", "2.0.0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upgrade := versionedPluginHostConfig(t, dir, "2.0.0")
			applyDone := make(chan struct{})
			go func() { h.ApplyConfig(ctx, upgrade); close(applyDone) }()
			waitForHostTestSignal(t, registerStarted, "replacement registration")
			cancel()
			waitForHostTestSignal(t, applyDone, "canceled replacement apply")

			h.ApplyConfig(context.Background(), &config.Config{})
			if h.PluginRegistered("alpha") {
				t.Fatal("global disable did not clear the snapshot")
			}
			betaPath := writeVersionedPluginFile(t, dir, "beta", "1.0.0")
			reenable := versionedPluginHostConfig(t, dir, "1.0.0")
			reenable.Plugins.Configs["beta"] = enabledPluginConfigWithStoreVersion(t, "1.0.0")
			h.ApplyConfig(context.Background(), reenable)
			unblockRegister()
			waitForHostTestSignal(t, rollbackStarted, "old plugin rollback")

			latest := versionedPluginHostConfig(t, dir, "1.0.0")
			alphaConfig := latest.Plugins.Configs["alpha"]
			alphaConfig.Priority = 17
			latest.Plugins.Configs["alpha"] = alphaConfig
			latest.Plugins.Configs["beta"] = enabledPluginConfigWithStoreVersion(t, "1.0.0")
			wantAlpha, wantBeta := false, true
			switch latestIntent {
			case "reenable":
				wantAlpha = true
			case "disable all", "shutdown":
				wantBeta = false
			case "disable plugin":
				enabled := false
				alphaConfig.Enabled = &enabled
				latest.Plugins.Configs["alpha"] = alphaConfig
			case "unload unrelated":
				wantAlpha, wantBeta = true, false
			}
			intentApplied := make(chan struct{})
			go func() {
				switch latestIntent {
				case "disable all":
					h.ApplyConfig(context.Background(), &config.Config{})
				case "unload plugin":
					h.UnloadPluginContext(ctx, "alpha")
				case "shutdown":
					h.ShutdownAllContext(ctx)
				case "unload unrelated":
					h.ApplyConfig(context.Background(), latest)
					h.UnloadPlugin("beta")
				default:
					h.ApplyConfig(context.Background(), latest)
				}
				close(intentApplied)
			}()
			waitForHostTestSignal(t, intentApplied, "latest config while native rollback is blocked")
			if old.shutdownCalls.Load() != 0 {
				t.Fatal("old plugin was physically unloaded during its native rollback")
			}
			unblockRollback()
			waitForHostLoadCleanup(t, h, "alpha")

			if got := h.PluginRegistered("alpha"); got != wantAlpha {
				t.Fatalf("PluginRegistered(alpha) = %t, want latest intent %t", got, wantAlpha)
			}
			if got := h.pluginIdentityCurrent("alpha", paths["1.0.0"], "1.0.0"); got != wantAlpha {
				t.Fatalf("old plugin active identity = %t, want %t", got, wantAlpha)
			}
			wantThinkingOwner := ""
			if wantAlpha {
				wantThinkingOwner = "alpha"
			} else if wantBeta {
				wantThinkingOwner = "beta"
			}
			gotThinkingOwner := ""
			if applier, ok := thinking.GetProviderApplier("rollback-thinking").(*thinkingAdapter); ok {
				gotThinkingOwner = applier.pluginID
			}
			if gotThinkingOwner != wantThinkingOwner {
				t.Fatalf("thinking provider owner = %q, want %q", gotThinkingOwner, wantThinkingOwner)
			}
			if got := h.PluginRegistered("beta"); got != wantBeta {
				t.Fatalf("PluginRegistered(beta) = %t, want %t; rollback overwrote unrelated state", got, wantBeta)
			}
			if got := h.pluginIdentityCurrent("beta", betaPath, "1.0.0"); got != wantBeta {
				t.Fatalf("unrelated plugin active identity = %t, want %t", got, wantBeta)
			}
			if wantAlpha {
				records := h.Snapshot().records
				if len(records) == 0 || records[0].id != "alpha" || records[0].priority != 17 {
					t.Fatal("rollback did not reconcile the latest priority and snapshot ordering")
				}
			}
			if got := rollbacks.Load(); got != 1 {
				t.Fatalf("native rollback calls = %d, want exactly one", got)
			}
			if replacement.shutdownCalls.Load() != 1 {
				t.Fatal("replacement did not shut down exactly once")
			}
		})
	}
}
