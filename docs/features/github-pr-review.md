# Pull Request Review & Agent Collaboration

px0 checks out a pull request's full source tree and lets you review it like any local workspace: full codebase navigation, symbol outline, LSP intelligence and search, alongside a diff scoped to the PR's merge-base. On top of that you get inline comment drafting, AI batch application, review verdicts, conflict resolution by your coding harness, and merging. The same git panel a plain workspace has lets you commit and push fixes back to the PR's own branch, and pull in commits someone else pushed while you were reviewing.

It works with **GitHub**, **GitLab** (gitlab.com and self-hosted), **Gitea / Forgejo / Codeberg**, and **Bitbucket Cloud**.

> [!TIP]
> New to PR review in px0? [Getting Started with Pull Request Review](pr-review-getting-started.md) walks through the first review end to end, including connecting your forge.

---

## Overview & Core Purpose

A forge's web PR view shows you the isolated diff, but not the codebase around it. Jumping to a caller three files away, or checking how a changed function is used elsewhere, means either trusting memory or cloning and switching branches by hand.

With px0, you pass the pull request URL:
```bash
px0 https://github.com/owner/repo/pull/123
```

px0 prepares a temporary worktree or clone, computes the merge-base diff against the target branch, and opens a lightweight code viewer in your browser.

---

## Opening a Pull Request

Pass the full URL:

```bash
px0 https://github.com/owner/repo/pull/123
px0 https://gitlab.com/group/subgroup/project/-/merge_requests/45
px0 https://codeberg.org/owner/repo/pulls/7
px0 https://bitbucket.org/workspace/repo/pull-requests/9
```

| Forge | URL shape | Notes |
| :--- | :--- | :--- |
| GitHub | `github.com/<owner>/<repo>/pull/<n>` | |
| GitLab | `<host>/<group>[/<subgroup>...]/<project>/-/merge_requests/<n>` | gitlab.com or any self-hosted instance; nested groups are supported |
| Gitea / Forgejo | `<host>/<owner>/<repo>/pulls/<n>` | Codeberg, gitea.com, or any self-hosted instance |
| Bitbucket Cloud | `bitbucket.org/<workspace>/<repo>/pull-requests/<n>` | Bitbucket Data Center / Server is not supported |

Trailing paths such as `/files`, `/diffs` or `/diff` are fine. Everything in this document works on every forge. Where a forge behaves differently, [Forge Differences](#forge-differences) says how.

> [!NOTE]
> Bare PR numbers (e.g. `px0 123`) and the `px0 pr` subcommand have been deprecated in favor of explicit URL routing (`px0 <url>`). Running `px0 pr` provides a helpful reminder to pass the URL directly.

### Interactive Preparation Spinner
Fetching metadata and checking out remote references takes a few moments, so px0 shows an animated CLI spinner:
```text
⠋ Fetching PR #123 metadata from github...
⠙ Fetching PR #123 head and preparing worktree...
⠸ Computing merge base with main...
✔ PR #123 checked out (Refactor auth token resolution)
```

### Merged PR Handling
If the pull request is already merged, px0 opens it directly without blocking, and shows a purple **`Merged`** badge in both the CLI checkout message and the browser review header.

### Multi-Session Isolation
Each PR review runs as its own isolated process on its own port. Running `px0 https://github.com/owner/repo/pull/456` while another PR review or local workspace is open will not disturb existing sessions.

From inside a running px0 browser session, the Command Palette (`Cmd/Ctrl+K` → **Git: Open Pull Request…**) launches a new review process in a fresh browser tab. In a workspace opened from the [workbench](workbench.md), the PR opens as another workbench workspace instead.

---

## Connecting Your Forge

Public repositories open, diff and navigate without any token. Posting comments, submitting reviews, pushing, and merging need one.

When px0 has no token, the PR bar shows **No &lt;forge&gt; token — click to connect**. Clicking it, or trying anything that needs a token (posting a comment, replying, submitting a review), opens the **Connect** dialog:
1. A link to the token page on the PR's own host, for example `gitlab.example.com/-/user_settings/personal_access_tokens`.
2. What to choose on that page: the scopes or permissions px0 needs.
3. A field to paste the token into (for a Bitbucket API token, also the account email).

px0 checks the token by reading the PR with it before keeping it. A token the forge rejects is never saved. An accepted token is saved to your px0 settings and **applies to the running review at once**: no restart needed. If you lack push access to the repository, px0 tells you that Approve and Merge will stay unavailable.

The step-by-step token instructions for each forge are in [Getting Started](pr-review-getting-started.md#2-connect-your-forge).

### Where px0 looks for a token

The first match wins.

| Forge | 1. px0 Settings | 2. Environment | 3. CLI |
| :--- | :--- | :--- | :--- |
| GitHub | `github.token` | `GITHUB_TOKEN`, then `GH_TOKEN` | `gh auth token` |
| GitLab | `gitlab.token` | `GITLAB_TOKEN` | the token `glab auth login` stored for the MR's host |
| Gitea / Forgejo | `gitea.token` | `GITEA_TOKEN` | — |
| Bitbucket Cloud | `bitbucket.token` (+ `bitbucket.email` for an API token) | `BITBUCKET_TOKEN` (+ `BITBUCKET_EMAIL`) | — |

### Self-hosted instances and token safety

A PR URL can name any host, so px0 never sends a token to a host it wasn't configured for:
- **GitLab**: `gitlab.token` and `GITLAB_TOKEN` go only to `gitlab.host` (Settings), else `GITLAB_HOST`, else gitlab.com. Other instances use the token glab stored for them.
- **Gitea / Forgejo**: `gitea.token` and `GITEA_TOKEN` go only to `gitea.host` (Settings), else `GITEA_HOST`, else codeberg.org.
- GitHub and Bitbucket Cloud each have a single host.

Connecting through the dialog saves the host along with the token. If a token for a *different* host of the same forge is already saved, the new one is used for the current session only and the saved one is left alone.

Tokens are stored in your px0 settings file (`~/.px0/settings.json`, or `$XDG_CONFIG_HOME/px0/settings.json`) on the machine running px0. Git receives them through environment variables scoped to the forge's URL, never on the command line, in remote config, or in URLs, and px0 strips them from any error text it shows.

### Without a token
- Public repositories still check out, diff, and navigate.
- You can draft review comments locally and use **⚡ Batch Apply** with local AI agents.
- The CLI banner says which forge needs a token and where px0 would find one, for example:
  ```text
  access: read-only (no GitHub token: set GITHUB_TOKEN, set GH_TOKEN, or run `gh auth login` to submit reviews)
  ```

---

## Reviewing & Real-Time Agent Collaboration

### 1. Merge-Base Diff View
- The **Changes** toggle in the sidebar defaults to all files modified by the PR.
- Press **`Cmd/Ctrl+D`** on any file to open side-by-side or unified diffs.
- The diff is computed against the merge-base between the PR head and its target branch, the same diff the forge shows.

### 2. Live Agent Synchronization
When you or your background AI coding agents (Claude Code, Gemini CLI, Cursor Agent, Antigravity, Aider, etc.) make edits to the PR checkout from other terminals:
- px0's real-time file watcher picks up modifications without full re-indexing.
- Gutter diffs, status badges, and open tabs reload live in the browser.

### 3. Line Comments & Hover Actions
When hovering over code lines or diff lines:
- A thread icon appears next to the line number.
- Clicking it opens a context menu for that line. On a line that is part of the PR's diff it includes:
  - **Add Review Comment**: Drafts an inline review comment.
  - **Edit Inline**: Prompts your local AI coding agent to edit those lines directly.
  - **Start Thread**: Opens a conversation with the agent about the line.
- Alternatively, select any range of lines and press **`Alt+R`** (or click **Comment** on the selection bar) to open the review comment composer.

### 4. Batch Applying Comments Locally (`⚡ Batch Apply`)
Drafted review comments appear in the PR top bar and inline across files:
- **⚡ Batch Apply** applies all drafted review comments across the whole pull request in one go, using your configured coding agent harness. The agent reads your comments as instructions and changes the code directly in the PR worktree.
- Comments stay drafts until they are batch-applied or submitted. Drafts are saved outside the repository, per pull request, so reopening the same PR, with `px0 <pr-url>` or from the [workbench](workbench.md), brings them back.

### 5. Submitting Reviews
The PR review bar at the bottom of the screen (right above the comments panel) holds the review summary and the verdict buttons:
- **Submit Review**: feedback without an approval status (available to every reviewer with a token).
- **Approve** / **Request Changes**: available when your token has push access to the repository.
- Submitting sends every draft comment and the summary together. How each forge records them is in [Forge Differences](#forge-differences).
- The verdict follows the branch. While a rebase is running or stopped halfway in the checkout, every verdict button is disabled. Once the checkout is rebased but not yet force-pushed, **Approve** stays disabled: the forge still holds the old commits, and approving now would approve those. When the forge reports conflicts, **Approve** still works but shows a dashed border and a tooltip saying the PR can't merge until the conflicts are resolved.

### 6. Merge Status, Resolving Conflicts, and Merging
The first row of the PR bar shows a **status pill** with the forge's answer to "can this merge?": *Ready to merge*, *Checks failing*, *Conflicts with main*, *Behind main*, *Merge blocked*, *Draft*, or *Checking mergeability...* (px0 asks again a few times while the forge computes it). The checkout's own state takes priority over the forge's: *Rebasing...*, *Rebase unfinished*, or *Rebased, not pushed*. Clicking the pill or the **Rebase with AI** button next to it takes the next step (resolve or force-push). When there is no next step, clicking the pill refreshes the status.

**Resolving conflicts / updating the branch.** When the PR conflicts with its base, or is behind it, click **Rebase with AI** (or click the status pill). This opens the git panel's [agent rebase flow](git-integration.md#catching-up-with-a-moving-remote), aimed at the PR's base:
1. px0 fetches the base branch itself, with your forge token, so the harness never needs credentials and never fetches.
2. The git panel's notice card asks for permission: **Rebase with AI** (replays the PR's commits on top of the base) or **Merge with AI** (brings the base in with a merge commit). Pick the harness and model right there. Nothing runs until you click.
3. The harness resolves conflicts and finishes the operation. It never pushes. **Cancel** stops it, and a rebase stopped halfway can be finished by the harness or aborted.
4. After a merge, **Push to PR** is an ordinary push. After a rebase, the PR's commits have been rewritten, so the only way to update the PR is **Force-push**, and px0 asks for confirmation first. The push uses `--force-with-lease` against the head the checkout last saw. If anyone else pushed to the branch in the meantime, the push is refused and their commits survive.

**Merging.** With push access, a split merge button sits next to **Approve**. The main button runs the method you used last (e.g. **Squash and merge**). The caret lists every method the repository allows, with a one-line description of each: **Create a merge commit**, **Squash and merge**, **Rebase and merge**. Clicking the merge button opens the merge dialog:
- A method switcher. Edits made under one method are kept when you switch to another.
- When **Squash and merge** is selected, a checkbox lets you **Rewrite commit message with AI** to automatically generate an informative commit summary and description using your coding harness.
- For a merge commit or a squash: an editable **commit title** and **message**, prefilled with the usual defaults (`Title (#N)` and a list of the PR's commits for a squash; `Merge pull request #N from branch` for a merge commit). **Rewrite with AI** can also rewrite them from the PR's commits and diff on demand. It follows your [Commit Message Instructions](settings-and-configuration.md) from Settings plus any one-off instruction you type next to the button, and it rewrites the current text rather than starting from scratch. The result only fills the fields. Nothing merges until you confirm.
- For a rebase merge: the list of commits that will be replayed.
- Anything that makes merging now wrong blocks **Confirm**: a rebase in progress, a rebase not yet pushed, commits on the PR this checkout hasn't seen, or conflicts. Warnings that don't block are listed too: failing checks, and local commits that aren't pushed and won't be part of the merge.
- The merge is tied to the head commit you reviewed. If the PR moved since then, the merge is refused.

### 7. Editing, Committing, and Pushing Back
A PR checkout is a real git worktree, and the git panel works inside it exactly as it does in a plain workspace:
- **Edit and commit**: Make a change (by hand, or by dispatching a coding agent on the checkout), stage it, and either write a commit message yourself or click **Commit with AI**.
- **Push**: Sends the checkout's `HEAD` to the pull request's *actual* head branch on its actual repository — a fork included — not wherever the checkout happens to live locally.
- **Pull**: Re-fetches the PR's current head. If new commits landed on it since you opened the review and your checkout can fast-forward onto them cleanly, px0 updates the worktree and refreshes the diff, the PR bar, and the comments panel automatically. If your checkout has diverged — you made local commits that aren't on the PR head yet, or the head was force-pushed — px0 refuses with a clear message rather than merging; commit and push first, or resolve it in a terminal. (Bringing the PR up to date with its *base* branch is the separate **Resolve conflicts** flow above.)

> [!IMPORTANT]
> A PR checkout lives in a temporary directory for the life of the px0 process (see [Multi-Session Isolation](#multi-session-isolation) above). Any edits you make there — committed or not — are gone once the session closes, unless you've pushed them back to the PR's branch first.

---

## Forge Differences

The table shows where each forge differs. Details follow.

| | GitHub | GitLab | Gitea / Forgejo | Bitbucket Cloud |
| :--- | :--- | :--- | :--- | :--- |
| Self-hosted | — | yes | yes | no (Data Center unsupported) |
| Review submitted as | one review | separate threads + approval | one review | separate comments + approval |
| Request Changes | native | comment + withdrawn approval | native | native |
| Threaded replies | yes | yes | same-line comments | yes |
| Conflicts known in advance | yes | yes | yes | yes (from diffstat) |
| Protection rules known in advance | yes | yes | no (shown when merge is refused) | no (shown when merge is refused) |
| Merge pinned to reviewed commit | by the forge | by the forge | by the forge | checked by px0 just before |

**GitLab**
- **Review submission**: GitLab has no single call for submitting a review. Each draft becomes a diff thread, and the summary becomes a comment. **Approve** uses GitLab's approval, pinned to the commit you reviewed. **Request Changes** posts a "Changes requested" comment and withdraws your approval, if you had given one. A draft on an unchanged context line, which GitLab can't anchor from the data px0 has, is posted as a comment that names the `path:line`.
- **Merge methods** follow the project's settings. A project that uses merge commits (including semi-linear history) offers **Create a merge commit**. A project that uses fast-forward merges offers **Rebase and merge**, which lands the commits as they are. **Squash and merge** is offered unless squashing is disabled, and it is the only option when the project requires squashing. When the project requires a rebase first, the pill reads *Behind main*, and **Update branch** runs the agent rebase.
- **Threads**: general comment threads show as a flat list in the Conversation section.
- **UI wording**: px0 labels merge requests "PR #N" rather than GitLab's "!N".

**Gitea / Forgejo**
- **Review submission** works like GitHub's: one review that carries every draft and the verdict.
- **Replies**: Gitea's API has no reply-to-comment call. A reply is a new comment on the same line, which Gitea's own UI shows in the same conversation. Gitea doesn't report which side of the diff a comment is on, so existing comments are shown on the new side.
- **Merge status** is only "mergeable or not". Draft PRs are recognized by their title prefix (`WIP:`, `Draft:`, ...). Branch protection isn't reported in advance; it shows up as the merge being refused, with Gitea's reason.
- **Merge methods** follow the repository settings. When rebase is disabled but fast-forward-only is enabled, **Rebase and merge** uses fast-forward-only.

**Bitbucket Cloud**
- **Checkout**: Bitbucket publishes no ref for a PR head, so px0 clones the source branch, from the fork when there is one, even inside a local clone.
- **Review submission**: each draft is posted as an inline comment and the summary as a comment. **Approve** and **Request Changes** use Bitbucket's own approval and request-changes states. Replies are threaded natively.
- **Merge status**: conflicts are detected from the PR's diffstat. Merge checks such as required approvals or builds aren't reported in advance; they show up as the merge being refused, with Bitbucket's reason.
- **Merge methods** come from the destination branch's allowed strategies: merge commit, squash, and rebase (`rebase_fast_forward`, or `fast_forward` if that's all that's allowed).
- **Head check**: Bitbucket's merge call can't be pinned to a commit, so px0 re-reads the PR head just before merging and refuses if it moved. A push in the moment between the check and the merge isn't caught.
- **Tokens**: an access token (repository, project or workspace) or an Atlassian API token plus the account email. Only an API token can check push access, so **Approve** and **Merge** stay hidden with an access token.

---

## Extensible Forge Architecture (`GitProvider`)

px0 abstracts forge interactions through the `GitProvider` interface in [`forge.go`](../../forge.go):
- **Provider Detection**: Matches input URLs against registered providers (GitHub, GitLab, Gitea/Forgejo, Bitbucket Cloud).
- **Normalized Metadata**: Maps forge-specific PR/MR objects to standard `PRMeta` structures.
- **Push Access & Token Discovery**: Isolates provider-specific authentication, including which host a token may be sent to.
- **Onboarding**: Each provider describes how to create its token (`TokenSetup`), which is what the Connect dialog shows.
- **Git Transport**: Each provider supplies the clone URL, the ref its PR heads are published under, and how git authenticates, so checkout, pull, push, and rebase are forge-neutral.
- **Merging**: `MergeStatus` and `Merge` speak forge-neutral merge states (`clean`, `conflicts`, `behind`, `blocked`, ...) and methods (`merge`, `squash`, `rebase`). Each provider maps them onto its own API, so the pill, the split button, and the dialog work unchanged for any forge.

Implementation details are in [the internals guide](../internals/github-pr-review.md).

---

## Keyboard Shortcuts & Controls

| Shortcut / Control | Context | Action |
| :--- | :--- | :--- |
| `Alt+R` | Selection in editor or diff view | Open review comment composer |
| Line Hover (`✏`) | Hovering on editor line number | Choose between a review comment, an inline agent edit, or a thread |
| `Cmd/Ctrl+D` | Active tab | Toggle side-by-side / unified diff against merge-base |
| Command Palette (`Cmd/Ctrl+K`) | Command Palette → **Git: Open Pull Request…** | Launch a new PR review tab |
| **No token — click to connect** | PR header bar | Open the Connect dialog |
| `Enter` / `Esc` | Connect dialog | Check and save the token / cancel |
| **`⚡ Batch Apply`** | PR header bar | Dispatch all drafted comments to local AI coding harness |
| **Submit Review** | PR header bar | Submit Approve / Request Changes / Comment to remote forge |
| Status pill | PR header bar | Show whether the PR can merge; click for the next step (resolve, force-push) or to refresh |
| **Squash and merge ▾** | PR header bar | Merge with the last-used method (caret: pick another); opens the merge dialog |
| `Cmd/Ctrl+Enter` / `Esc` | Merge dialog | Confirm the merge / cancel |
| **Resolve conflicts** | PR header bar | Rebase onto, or merge, the base branch with the coding harness (asks first) |
| **Force-push** | Sidebar git panel | After a rebase: replace the PR branch's commits, with a lease (asks first) |
| **Pull** | Sidebar git panel | Fast-forward the checkout onto the PR's current head; refuses on divergence |
| **Push** | Sidebar git panel | Push the checkout's `HEAD` to the PR's actual head branch |
| **Commit with AI** | Sidebar git panel | Write and commit a message for the staged diff with your coding harness |
