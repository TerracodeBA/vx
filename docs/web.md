# The web UI on a phone

You follow your agents from a phone the same way as from a laptop: the web UI at `/ui` fits screens from 360 px wide. Sign in on the phone with GitHub, or with a token of your own (`vx token create`).

## Which account is signed in

The sidebar (the menu on a phone) shows, on every page, the account you are signed in as and its GitHub login (`GitHub: LOGIN`, or "GitHub: not connected").

When the account can read no repositories, Repositories, All efforts and For you do not just show nothing: they say "You are signed in as HANDLE (GitHub: LOGIN). This account cannot read any repositories.", why when vx knows (the account was created minutes ago, which usually means a sign-in with another GitHub account than last time; it belongs to no organization besides its personal one; the vx GitHub App is not installed on any GitHub organization the account is in), and what to do: sign out and sign in with the other GitHub account (switch accounts on github.com first: GitHub keeps its own session), ask an owner of the organization for access, or install the GitHub App. As soon as the account reads one repository, the pages are back to normal.

Signing in with GitHub for the first time asks "You are about to create a new vx account for GitHub LOGIN", with **Continue** and **Not you? Switch GitHub account**, before it creates anything. People with an invite are not asked. See [signing in with the right GitHub account](getting-started.md#signing-in-with-the-right-github-account).

## For you

**For you** (`/ui/for-you`, first in the menu) is what waits for you, newest first:

- questions escalated to you or your agents (nobody answered in time), and questions asked of you directly;
- changes waiting for your approval under a `human` landing policy, when you may decide them;
- failed CI and pull request review comments on shipments of your agents' changes;
- overlap notes and needs-rebase notes on the efforts you own.

Answer a question, or approve or reject a review, right there: the forms post to this server only, and only people (not agents) can use them. The page shows only what is in repositories you can read, like `vx inbox` and the Reviews page.

## Home screen

Add vx to the home screen (Android: browser menu, Install app or Add to Home screen; iPhone and iPad: Share, Add to Home Screen). It opens on For you, full screen. Without a connection it shows a "you are offline" page; vx keeps nothing on the device.

## Notifications

Notifications are off until you turn them on, per browser: **Settings > Notifications > Turn on in this browser**. Then this device gets a notification when

- a question to you or to one of your agents escalates (its deadline passes without an answer), and
- a change waits for your approval under a landing policy.

Each thing is notified once; more than three at once arrive as one summary. Things from before you turned notifications on are not sent; they are on For you. **Send a test** checks the device, and Settings lists every browser that has notifications on, with a **Turn off** button each. On iPhone and iPad, notifications work in vx added to the Home Screen (iOS 16.4 or later), not in a Safari tab.

The server signs notifications with its own key (Web Push with VAPID), made on first use and stored encrypted with `VX_SECRET_KEY`; a server without `VX_SECRET_KEY` says so in Settings and sends none. The server posts notifications to the browser's push service (Google, Mozilla, Apple, Microsoft) over HTTPS, encrypted so the push service cannot read them; it refuses push endpoints that are not public HTTPS addresses. A server checks for new things to send every 30 seconds while it runs.
