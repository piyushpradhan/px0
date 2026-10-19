# Getting Started with Pull Request Review

This guide takes you from a pull request URL to a merged PR in px0: open it, connect your forge, review, fix, bring it up to date, and merge. It takes about five minutes the first time; after that, opening a PR is one command.

It works the same on **GitHub**, **GitLab** (gitlab.com and self-hosted), **Gitea / Forgejo / Codeberg**, and **Bitbucket Cloud**. Where a forge differs, this guide says so. The full reference is [Pull Request Review](github-pr-review.md).

**What you need**
- `git` installed.
- For conflict resolution, AI commit messages and batch-applying comments: a coding harness px0 supports (Claude Code, Gemini CLI, Cursor Agent, Codex, and others; see [Editing with Coding Agents](agent-editing.md)). Everything else works without one.

---

## 1. Open the pull request

Copy the PR's URL from your browser and pass it to px0:

```bash
px0 https://github.com/owner/repo/pull/123
px0 https://gitlab.com/group/project/-/merge_requests/45
px0 https://codeberg.org/owner/repo/pulls/7
px0 https://bitbucket.org/workspace/repo/pull-requests/9
```

px0 checks out the PR into a temporary directory and opens it in your browser. Run it from inside a clone of the repository if you have one: px0 then adds a worktree to that clone instead of cloning again, which is much faster for large repositories.

You can also open a PR from a running px0 (`Cmd/Ctrl+K` → **Git: Open Pull Request…**) or from the [workbench](workbench.md).

> [!NOTE]
> The checkout is temporary. It is removed when px0 exits, so push any commits you want to keep (step 4).

---

## 2. Connect your forge

You can read any public PR without a token. To comment, review, push, or merge, px0 needs one.

**The quick way:** click **No &lt;forge&gt; token — click to connect** in the PR bar. The Connect dialog links to the token page on your forge, lists what to pick there, and checks the token when you paste it. The token is saved and takes effect immediately, so there is nothing to restart.

The same steps, per forge:

### GitHub
1. Open [github.com/settings/personal-access-tokens/new](https://github.com/settings/personal-access-tokens/new) (a fine-grained token).
2. Name it (e.g. `px0`) and pick an expiration.
3. Under **Repository access**, choose the repositories you review, or **All repositories**.
4. Under **Repository permissions**, set **Contents** and **Pull requests** to **Read and write**.
5. Generate the token and paste it into px0.

Already use the GitHub CLI? If `gh auth login` has been run, px0 picks up its token automatically. `GITHUB_TOKEN` or `GH_TOKEN` work too.

### GitLab
1. Open **User settings → Access tokens** on your GitLab: `https://<your-gitlab>/-/user_settings/personal_access_tokens` (on gitlab.com: [gitlab.com/-/user_settings/personal_access_tokens](https://gitlab.com/-/user_settings/personal_access_tokens)).
2. Select **Add new token**, name it, and pick an expiration date.
3. Under **Select scopes**, check **`api`**.
4. Create the token and paste it into px0.

Self-hosted: the dialog remembers which instance the token belongs to (the `gitlab.host` setting), and px0 sends it only there. The GitLab CLI works too: after `glab auth login --hostname <your-gitlab>`, px0 finds the token glab stored for that host. Or set `GITLAB_TOKEN`, plus `GITLAB_HOST` for a self-hosted instance.

### Gitea, Forgejo, Codeberg
1. Open **Settings → Applications** on your instance: `https://<your-instance>/user/settings/applications` (on Codeberg: [codeberg.org/user/settings/applications](https://codeberg.org/user/settings/applications)).
2. Under **Generate new token**, name it.
3. Under **Select permissions**, set **repository** and **issue** to **Read and write**.
4. Generate the token and paste it into px0.

The dialog remembers the instance (the `gitea.host` setting), and px0 sends the token only there. Or set `GITEA_TOKEN`, plus `GITEA_HOST` for an instance other than codeberg.org.

### Bitbucket Cloud
Bitbucket offers two kinds of token. Use an **API token** if you want to approve and merge:

1. Open [id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens).
2. Select **Create API token with scopes**, name it, and pick an expiry.
3. Choose **Bitbucket** as the app.
4. Grant read and write access to repositories and pull requests, and read access to your account. px0 reads your account to check push access.
5. Paste the token into px0, along with your **Atlassian account email**.

A **repository, project or workspace access token** (created in the repository's or workspace's settings under **Access tokens**) also works: leave the email empty. Bitbucket doesn't let an access token check your permissions, though, so with one px0 can't confirm push access and keeps **Approve** and **Merge** hidden.

Environment variables: `BITBUCKET_TOKEN`, plus `BITBUCKET_EMAIL` for an API token.

### Where the token lives
Tokens are saved in your px0 settings file (`~/.px0/settings.json`, or `$XDG_CONFIG_HOME/px0/settings.json`) on the machine running px0, and can be changed under Settings (`Cmd/Ctrl+,`) → the forge's section. px0 sends a token only to the host it was set up for. Git gets the token through environment variables, never on the command line or in a URL.

---

## 3. Review

- **See the change in context.** The sidebar's **Changes** view lists the files the PR touches; `Cmd/Ctrl+D` toggles a file's diff against the PR's merge-base. Go to definition (`F12`), find references, and search all work across the whole repository, not just the diff.
- **Comment on a line.** Select lines and press `Alt+R` (or hover a line number and pick **Add Review Comment**). Comments are drafts until you submit them, and they survive closing px0.
- **Read and reply to the discussion.** The **Comments** panel at the bottom shows the PR's existing conversation and inline threads; reply in place.
- **Submit.** Write an optional summary in the PR bar, then **Submit Review**, **Request Changes**, or **Approve**. Your drafts go with it. Approve and Request Changes appear when your token has push access.

---

## 4. Fix things yourself (optional)

The checkout is a real git worktree with the full git panel:

- Edit a file (`Cmd/Ctrl+E`), or ask your coding harness to (`Alt+E` on a selection).
- **⚡ Batch Apply** turns all your draft comments into code changes in one go.
- Stage, then **Commit with AI** or write your own message, then **Push**. The push goes to the PR's own branch, even when it comes from a fork.
- If someone pushes to the PR while you review, **Pull** brings their commits in.

---

## 5. Bring the PR up to date with its base

When the base branch has moved on, the status pill in the PR bar shows **Conflicts with main** or **Behind main**, and the merge button becomes **Resolve conflicts** or **Update branch**. Click it:

1. px0 fetches the base branch and asks, in the git panel: **Rebase with AI** or **Merge with AI**? Pick the harness and model there.
2. The harness resolves any conflicts and finishes. It never pushes.
3. Review what it did, then push. A merge is a normal **Push to PR**. A rebase rewrites the PR's commits, so it needs a **Force-push**, which px0 confirms first. The force-push is refused if anyone else pushed in the meantime, so their work is never lost.

While the rebase runs, or sits unpushed, **Approve** and **Merge** wait. Otherwise they would act on the PR's old commits.

---

## 6. Merge

When the pill says **Ready to merge**, use the split button next to Approve:

1. Pick the method from the caret: **Create a merge commit**, **Squash and merge**, or **Rebase and merge**. Only the methods your repository allows are listed, and px0 remembers your choice.
2. In the dialog, edit the commit title and message. **Rewrite with AI** drafts them from the PR's commits and diff, following your Commit Message Instructions (Settings → Git & Diff) and anything you type next to the button.
3. **Confirm**. px0 merges exactly the commit you reviewed. If the PR changed since, the merge is refused and px0 asks you to pull first.

---

## Troubleshooting

| What you see | Why | What to do |
| :--- | :--- | :--- |
| *&lt;Forge&gt; didn't accept this token* in the Connect dialog | The token is wrong, expired, or lacks access to this repository. | Create a new one with the scopes listed in [step 2](#2-connect-your-forge). |
| *404 Not Found* while opening a private PR | No token, or the token can't see the repository. | Connect a token first (Settings, or an environment variable), then run px0 again. |
| No **Approve** / **Request Changes** / merge button | Your token lacks push access to the repository. On Bitbucket, an access token can't confirm push access. | Use a token from an account with write access. On Bitbucket, use an API token with your email. |
| A self-hosted GitLab or Gitea token isn't used | The token is saved for a different host. | Set **GitLab Host** / **Gitea Host** in Settings to your instance, or reconnect from the PR's dialog. |
| *Connected for this session (a token for another host is already saved)* | You already have a token saved for another instance of the same forge, and px0 kept it. | Nothing, if that's what you want. To switch permanently, change the token and host in Settings. |
| *Merge blocked* | Branch protection: required approvals, checks, or resolved threads. | Hover the pill for the reason, and meet the requirement on the forge. |
| The merge is refused although the pill said ready | On Gitea and Bitbucket, protection rules only show up when merging. | The error names the rule. |
| *The PR has new commits since this checkout* | Someone pushed after you loaded the PR. | **Pull**, review the new commits, then merge. |
| *someone else pushed … since this checkout* on force-push | The lease protected their commits; your rebase exists only in this checkout. | Reopen the PR (`px0 <url>`) to pick up their commits, then **Resolve conflicts** again. |
| *diff base unresolved* badge, empty diff | px0 couldn't fetch the base branch. | Check that the token can read the repository, then **Pull** to retry. |
| **Rebase with AI** / **Rewrite with AI** missing | No coding harness is installed. | Install one (see [Editing with Coding Agents](agent-editing.md)). |

---

## Where next

- [Pull Request Review](github-pr-review.md): the full reference, including per-forge differences.
- [Git Awareness & Diffs](git-integration.md): the git panel in depth.
- [Settings & Configuration](settings-and-configuration.md): every setting, including the forge tokens.
