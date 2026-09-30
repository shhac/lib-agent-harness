package nativecli

// CodexBrowserArgs enables the browser features identified in codex-cli
// 0.159.2. These select an already configured browser bridge; they do not
// install it, enable desktop control, or change browser site permissions.
func CodexBrowserArgs() []string {
	return []string{"-c", "features.browser_use=true", "-c", "features.browser_use_external=true"}
}
