package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/aural-chat/aural-server/internal/buildinfo"
	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/mattn/go-isatty"
)

// ANSI color codes for banner rendering.
const (
	cReset      = "\x1b[0m"
	cBold       = "\x1b[1m"
	cDim        = "\x1b[2m"
	cGray       = "\x1b[90m"
	cCyan       = "\x1b[36m"
	cBrightCyan = "\x1b[96m"
	cBlue       = "\x1b[34m"
	cBrightBlue = "\x1b[94m"
	cGreen      = "\x1b[32m"
	cYellow     = "\x1b[33m"
	cMagenta    = "\x1b[35m"
	cWhite      = "\x1b[97m"
)

func shouldColor(noColorCfg bool) bool {
	if noColorCfg || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// PrintBanner outputs a rich ASCII logo and a structured summary of the server
// state, configuration, and storage statistics.
func PrintBanner(cfg *config.Config, stats store.ServerStats) {
	colored := shouldColor(cfg.Log.NoColor)

	var (
		cLogo1  = cBrightCyan
		cLogo2  = cCyan
		cLogo3  = cBrightBlue
		cLogo4  = cBlue
		cBorder = cGray
		cKey    = cCyan
		cVal    = cWhite
		cVer    = cGreen
		cEnd    = cReset
	)

	if !colored {
		cLogo1, cLogo2, cLogo3, cLogo4 = "", "", "", ""
		cBorder, cKey, cVal, cVer, cEnd = "", "", "", "", ""
	}

	scheme := "ws://"
	httpScheme := "http://"
	if cfg.TLS.Enabled {
		scheme = "wss://"
		httpScheme = "https://"
	}
	address := cfg.Address()

	logInfo := fmt.Sprintf("Console (%s / %s)", cfg.Log.Format, strings.ToUpper(cfg.Log.Level))
	if cfg.Log.File != "" {
		fileLevel := cfg.Log.FileLevel
		if fileLevel == "" {
			fileLevel = cfg.Log.Level
		}
		logInfo += fmt.Sprintf(" • File: %s (%s)", cfg.Log.File, strings.ToUpper(fileLevel))
	}

	channelInfo := fmt.Sprintf("%d total (%d text, %d voice, %d categories)",
		stats.TotalChannels, stats.TextChannels, stats.VoiceChannels, stats.Categories)

	memberInfo := fmt.Sprintf("%d registered (%d guests)", stats.RegisteredUsers, stats.GuestUsers)

	banner := fmt.Sprintf(`
%s  █████╗ ██╗   ██╗██████╗  █████╗ ██╗     
%s ██╔══██╗██║   ██║██╔══██╗██╔══██╗██║     
%s ███████║██║   ██║██████╔╝███████║██║     
%s ██╔══██║██║   ██║██╔══██╗██╔══██║██║     
%s ██║  ██║╚██████╔╝██║  ██║██║  ██║███████╗
%s ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝%s
  %sAural Voice & Chat Server%s • %s%s%s

%s  ┌─────────────────────────────────────────────────────────────┐%s
%s  │%s %sServer:%s     %-44s %s│%s
%s  │%s %sEndpoint:%s   %-44s %s│%s
%s  │%s %sWeb/HTTP:%s   %-44s %s│%s
%s  │%s %sDatabase:%s   %-44s %s│%s
%s  │%s %sVoice Mode:%s %-44s %s│%s
%s  │%s %sMembers:%s    %-44s %s│%s
%s  │%s %sChannels:%s   %-44s %s│%s
%s  │%s %sRoles:%s      %-44s %s│%s
%s  │%s %sLogging:%s    %-44s %s│%s
%s  └─────────────────────────────────────────────────────────────┘%s
`,
		cLogo1,
		cLogo1,
		cLogo2,
		cLogo3,
		cLogo4,
		cLogo4, cEnd,
		cBold, cEnd, cVer, buildinfo.Version, cEnd,
		cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(cfg.Server.Name, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(scheme+address, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(httpScheme+address, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(cfg.Database.Path, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(cfg.Voice.Mode, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(memberInfo, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(channelInfo, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(fmt.Sprintf("%d roles configured", stats.TotalRoles), 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, truncatePad(logInfo, 44, cVal, cEnd), cBorder, cEnd,
		cBorder, cEnd,
	)

	fmt.Print(banner)
}

func truncatePad(val string, width int, valColor, resetColor string) string {
	if len(val) > width {
		val = val[:width-3] + "..."
	}
	padding := width - len(val)
	if padding < 0 {
		padding = 0
	}
	return valColor + val + resetColor + strings.Repeat(" ", padding)
}

// PrintOwnerToken puts the token on stdout with clear formatting so operators
// notice and can easily copy it.
func PrintOwnerToken(token string, noColor bool) {
	colored := shouldColor(noColor)
	cBorder := cYellow
	cKey := cBold + cYellow
	cVal := cBrightCyan + cBold
	cDimText := cGray
	cEnd := cReset

	if !colored {
		cBorder, cKey, cVal, cDimText, cEnd = "", "", "", "", ""
	}

	tokenFormatted := truncatePad(token, 45, cVal, cEnd)

	output := fmt.Sprintf(`
%s  ┌─────────────────────────────────────────────────────────────┐%s
%s  │%s %s[!] ONE-TIME OWNER TOKEN GENERATED%s                        %s│%s
%s  ├─────────────────────────────────────────────────────────────┤%s
%s  │%s                                                             %s│%s
%s  │%s   %sToken:%s %s %s│%s
%s  │%s                                                             %s│%s
%s  │%s   %sRedeem it once from a connected client to claim ownership. %s│%s
%s  │%s   %sRun with -new-owner-token to issue a replacement.         %s│%s
%s  └─────────────────────────────────────────────────────────────┘%s
`,
		cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, cBorder, cEnd,
		cBorder, cEnd,
		cBorder, cEnd, cBorder, cEnd,
		cBorder, cEnd, cKey, cEnd, tokenFormatted, cBorder, cEnd,
		cBorder, cEnd, cBorder, cEnd,
		cBorder, cEnd, cDimText+"Redeem it once from a connected client to claim ownership."+cEnd, cBorder, cEnd,
		cBorder, cEnd, cDimText+"Run with -new-owner-token to issue a replacement."+cEnd, cBorder, cEnd,
		cBorder, cEnd,
	)

	fmt.Print(output)
}
