package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

// DoChatGPTLogin runs the "Sign in with ChatGPT" login flow.
//
// The flow has no loopback listener: it prints the authorization URL, waits for the
// operator to paste the callback URL the browser ended up on, and stores the resulting
// credential in the configured auth directory.
func DoChatGPTLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	manager := newAuthManager()

	_, savedPath, err := manager.Login(context.Background(), chatgpt.ProviderName, cfg, &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Prompt:    promptFn,
	})
	if err != nil {
		fmt.Printf("ChatGPT authentication failed: %v\n", err)
		return
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	fmt.Println("Sign in with ChatGPT completed; use the model names published for this provider.")
}
