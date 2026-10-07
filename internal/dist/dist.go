// Package dist is the single source for where CoSpace is published: install
// commands shown to guests and hosts, release downloads, and the base image.
// Everything lives on GitHub (Pages, Releases, ghcr.io), so the project runs
// no download server of its own.
package dist

const (
	// Repo is the GitHub repository that publishes releases.
	Repo = "JingxuanKang/cospace"
	// Pages serves the install scripts.
	Pages = "https://cospace.jingxuan.uk"

	GuestInstallPOSIX = "curl -fsSL " + Pages + "/install.sh | sh"
	// Pages serves .ps1 as application/octet-stream; raw.githubusercontent
	// serves it as text, which `irm | iex` handles on every PowerShell.
	GuestInstallWindows = "irm https://raw.githubusercontent.com/" + Repo + "/master/docs/install.ps1 | iex"
	HostInstall         = "curl -fsSL " + Pages + "/host.sh | sh"

	// BaseImage is the prebuilt space image this daemon release creates
	// spaces from. Bump the tag together with image/Dockerfile changes.
	BaseImage = "ghcr.io/jingxuankang/cospace-base:1"
)
