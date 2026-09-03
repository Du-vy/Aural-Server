package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// GitHub-shaped deliveries, accepted at /api/webhooks/{id}/{token}/github.
//
// GitHub posts its own event schema and nothing else: it cannot be asked to
// send a message, only to send what happened. Discord answers that path by
// rendering the event into a card, and a repository configured against a
// Discord webhook is configured against this one by changing the URL.
//
// The events below are the ones a repository actually generates in volume.
// Anything else still arrives, as a one-line card naming the event, because a
// delivery that renders plainly is better than a 400 GitHub will show as a
// failed hook.

// Colours, chosen to read the way the same events read on GitHub itself.
const (
	githubGreen  = 0x2cbe4e // opened, created, published
	githubRed    = 0xcb2431 // closed without merging, deleted, failed
	githubPurple = 0x6f42c1 // merged
	githubGrey   = 0x586069 // everything with no state of its own
	githubOrange = 0xdbab09 // in progress, requested changes
)

type githubUser struct {
	Login     string `json:"login"`
	HTMLURL   string `json:"html_url"`
	AvatarURL string `json:"avatar_url"`
}

type githubRepo struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type githubCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	URL     string `json:"url"`
	Author  struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"author"`
}

type githubIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	State   string `json:"state"`
}

type githubPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
}

type githubComment struct {
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

type githubReview struct {
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	State   string `json:"state"`
}

type githubRelease struct {
	Name       string `json:"name"`
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

type githubWorkflowRun struct {
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	HeadBranch string `json:"head_branch"`
}

// githubDelivery is every field the renderers below read. GitHub's payloads
// share one envelope and differ in which of these are present, so one struct
// covers them all rather than one per event.
type githubDelivery struct {
	Action  string `json:"action"`
	Zen     string `json:"zen"`
	Ref     string `json:"ref"`
	RefType string `json:"ref_type"`
	Compare string `json:"compare"`
	Forced  bool   `json:"forced"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`

	Commits     []githubCommit     `json:"commits"`
	Repository  *githubRepo        `json:"repository"`
	Sender      *githubUser        `json:"sender"`
	Issue       *githubIssue       `json:"issue"`
	PullRequest *githubPull        `json:"pull_request"`
	Comment     *githubComment     `json:"comment"`
	Review      *githubReview      `json:"review"`
	Release     *githubRelease     `json:"release"`
	Forkee      *githubRepo        `json:"forkee"`
	WorkflowRun *githubWorkflowRun `json:"workflow_run"`
}

// githubPayload turns one GitHub event into the delivery the rest of the
// pipeline posts.
func githubPayload(r *http.Request, body []byte) (executePayload, *deliveryFailure) {
	var in githubDelivery
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return executePayload{}, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
				"Invalid Form Body")
		}
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		return executePayload{}, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
			"Missing X-GitHub-Event header")
	}

	embed, ok := githubEmbed(event, in)
	if !ok {
		// GitHub retries a hook that failed and marks it red in the
		// repository's settings, so an event with nothing worth drawing is
		// accepted and dropped rather than refused.
		return executePayload{}, errDeliveryIgnored
	}
	if in.Sender != nil && in.Sender.Login != "" {
		embed.Author = &protocol.EmbedAuthor{
			Name:    in.Sender.Login,
			URL:     in.Sender.HTMLURL,
			IconURL: in.Sender.AvatarURL,
		}
	}
	return executePayload{Embeds: []protocol.Embed{embed}}, nil
}

// githubEmbed renders one event. The second result is false for an event that
// is deliberately not drawn at all.
func githubEmbed(event string, in githubDelivery) (protocol.Embed, bool) {
	repo := "repository"
	repoURL := ""
	if in.Repository != nil {
		if in.Repository.FullName != "" {
			repo = in.Repository.FullName
		}
		repoURL = in.Repository.HTMLURL
	}
	colour := func(c int) *int { return &c }

	switch event {
	case "ping":
		return protocol.Embed{
			Title:       fmt.Sprintf("[%s] Webhook connected", repo),
			Description: in.Zen,
			URL:         repoURL,
			Color:       colour(githubGrey),
		}, true

	case "push":
		branch := strings.TrimPrefix(in.Ref, "refs/heads/")
		if len(in.Commits) == 0 {
			// A branch created or deleted arrives as a push with nothing in
			// it; the create and delete events already say so.
			return protocol.Embed{}, false
		}
		lines := make([]string, 0, len(in.Commits))
		for _, c := range in.Commits {
			short := c.ID
			if len(short) > 7 {
				short = short[:7]
			}
			author := c.Author.Username
			if author == "" {
				author = c.Author.Name
			}
			line := fmt.Sprintf("[`%s`](%s) %s", short, c.URL, githubFirstLine(c.Message))
			if author != "" {
				line += " — " + author
			}
			lines = append(lines, line)
		}
		return protocol.Embed{
			Title:       fmt.Sprintf("[%s:%s] %s", repo, branch, githubPlural(len(in.Commits), "new commit")),
			Description: strings.Join(lines, "\n"),
			URL:         in.Compare,
			Color:       colour(githubGrey),
		}, true

	case "issues":
		if in.Issue == nil {
			return protocol.Embed{}, false
		}
		shade := githubGreen
		if in.Action == "closed" {
			shade = githubRed
		}
		body := ""
		if in.Action == "opened" {
			body = in.Issue.Body
		}
		return protocol.Embed{
			Title:       fmt.Sprintf("[%s] Issue %s: #%d %s", repo, in.Action, in.Issue.Number, in.Issue.Title),
			Description: githubExcerpt(body),
			URL:         in.Issue.HTMLURL,
			Color:       colour(shade),
		}, true

	case "issue_comment":
		if in.Issue == nil || in.Comment == nil || in.Action == "deleted" {
			return protocol.Embed{}, false
		}
		return protocol.Embed{
			Title:       fmt.Sprintf("[%s] New comment on issue #%d: %s", repo, in.Issue.Number, in.Issue.Title),
			Description: githubExcerpt(in.Comment.Body),
			URL:         in.Comment.HTMLURL,
			Color:       colour(githubGrey),
		}, true

	case "pull_request":
		if in.PullRequest == nil {
			return protocol.Embed{}, false
		}
		action, shade := in.Action, githubGreen
		switch in.Action {
		case "closed":
			if in.PullRequest.Merged {
				action, shade = "merged", githubPurple
			} else {
				shade = githubRed
			}
		case "opened", "reopened", "ready_for_review":
		default:
			// Labels, assignees and the rest are noise in a channel.
			return protocol.Embed{}, false
		}
		body := ""
		if action == "opened" {
			body = in.PullRequest.Body
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] Pull request %s: #%d %s",
				repo, action, in.PullRequest.Number, in.PullRequest.Title),
			Description: githubExcerpt(body),
			URL:         in.PullRequest.HTMLURL,
			Color:       colour(shade),
		}, true

	case "pull_request_review":
		if in.PullRequest == nil || in.Review == nil || in.Action != "submitted" {
			return protocol.Embed{}, false
		}
		state, shade := "reviewed", githubGrey
		switch in.Review.State {
		case "approved":
			state, shade = "approved", githubGreen
		case "changes_requested":
			state, shade = "requested changes on", githubOrange
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] Pull request %s: #%d %s",
				repo, state, in.PullRequest.Number, in.PullRequest.Title),
			Description: githubExcerpt(in.Review.Body),
			URL:         in.Review.HTMLURL,
			Color:       colour(shade),
		}, true

	case "pull_request_review_comment":
		if in.PullRequest == nil || in.Comment == nil || in.Action != "created" {
			return protocol.Embed{}, false
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] New review comment on #%d: %s",
				repo, in.PullRequest.Number, in.PullRequest.Title),
			Description: githubExcerpt(in.Comment.Body),
			URL:         in.Comment.HTMLURL,
			Color:       colour(githubGrey),
		}, true

	case "release":
		if in.Release == nil || (in.Action != "published" && in.Action != "released") {
			return protocol.Embed{}, false
		}
		name := in.Release.Name
		if name == "" {
			name = in.Release.TagName
		}
		return protocol.Embed{
			Title:       fmt.Sprintf("[%s] New release published: %s", repo, name),
			Description: githubExcerpt(in.Release.Body),
			URL:         in.Release.HTMLURL,
			Color:       colour(githubGreen),
		}, true

	case "create":
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] New %s created: %s", repo, githubRefType(in.RefType), in.Ref),
			URL:   repoURL,
			Color: colour(githubGreen),
		}, true

	case "delete":
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] %s deleted: %s", repo, githubTitleCase(githubRefType(in.RefType)), in.Ref),
			URL:   repoURL,
			Color: colour(githubRed),
		}, true

	case "fork":
		name := ""
		if in.Forkee != nil {
			name = in.Forkee.FullName
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] Fork created: %s", repo, name),
			URL:   repoURL,
			Color: colour(githubGrey),
		}, true

	case "star", "watch":
		if in.Action != "created" && in.Action != "started" {
			return protocol.Embed{}, false
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s] New star added", repo),
			URL:   repoURL,
			Color: colour(githubGrey),
		}, true

	case "workflow_run":
		if in.WorkflowRun == nil || in.Action != "completed" {
			return protocol.Embed{}, false
		}
		shade := githubGrey
		switch in.WorkflowRun.Conclusion {
		case "success":
			shade = githubGreen
		case "failure", "timed_out":
			shade = githubRed
		case "cancelled", "action_required":
			shade = githubOrange
		}
		return protocol.Embed{
			Title: fmt.Sprintf("[%s:%s] %s %s", repo, in.WorkflowRun.HeadBranch,
				in.WorkflowRun.Name, in.WorkflowRun.Conclusion),
			URL:   in.WorkflowRun.HTMLURL,
			Color: colour(shade),
		}, true

	default:
		// Something this server does not draw specially. The event is still
		// worth a line: an operator who wired up a hook wants to see it
		// arriving, not to wonder whether it did.
		title := fmt.Sprintf("[%s] %s", repo, githubTitleCase(strings.ReplaceAll(event, "_", " ")))
		if in.Action != "" {
			title += ": " + in.Action
		}
		return protocol.Embed{Title: title, URL: repoURL, Color: colour(githubGrey)}, true
	}
}

// githubFirstLine is a commit's subject, which is all a list of them shows.
func githubFirstLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return truncateRunes(strings.TrimSpace(line), 120)
}

// githubExcerpt is as much of a body as belongs in a card. The rest is one
// click away, on the page the card links to.
func githubExcerpt(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	const limit = 500
	if len([]rune(trimmed)) <= limit {
		return trimmed
	}
	return truncateRunes(trimmed, limit) + "…"
}

func githubPlural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// githubRefType names what a create or delete event was about.
func githubRefType(refType string) string {
	if refType == "" {
		return "ref"
	}
	return refType
}

func githubTitleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
