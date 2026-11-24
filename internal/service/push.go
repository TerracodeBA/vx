package service

// Web Push notifications (v0.5, #37), opt-in per person and device: a
// person turns them on in Settings, their browser subscribes with the
// server's VAPID key, and a background loop pushes questions escalated to
// them (or to their agents) and reviews they may approve. Each thing is
// pushed once (push_sent), by whichever server instance claims it first.

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/secret"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/webpush"
)

// PushSettings configures Web Push delivery.
type PushSettings struct {
	// Every is how often the notifier looks for new things to push; zero:
	// DefaultPushEvery.
	Every time.Duration
	// Client posts to push services; nil: webpush.PublicClient, which only
	// connects to public addresses. Tests set it.
	Client *http.Client
	// AllowAnyEndpoint accepts http and private endpoints (tests only).
	AllowAnyEndpoint bool
}

const (
	// DefaultPushEvery is how often the notifier runs.
	DefaultPushEvery = 30 * time.Second
	// pushWindow: things older than this are not pushed (they are on the
	// For you page); push_sent rows older than pushForget are dropped.
	pushWindow = 3 * 24 * time.Hour
	pushForget = 30 * 24 * time.Hour
	// pushBatch: more new things than this for one person in one run make
	// a single summary notification.
	pushBatch = 3
	// maxPushDevices bounds the subscriptions of one person.
	maxPushDevices = 20
	pushPurpose    = "push.vapid_key"
	pushTTL        = 24 * time.Hour
)

// PushAvailable reports whether this server can send notifications: it
// needs VX_SECRET_KEY to keep its VAPID key.
func (s *Service) PushAvailable() bool { return s.CanSealSecrets() }

// vapidKey returns the server's VAPID key, making it on first use.
func (s *Service) vapidKey(ctx context.Context) (*ecdsa.PrivateKey, string, error) {
	key, err := s.secretKey()
	if err != nil {
		return nil, "", errf(CodeFailedPrecondition, "notifications need VX_SECRET_KEY on the server, to keep the key they are signed with")
	}
	row, err := s.Store.PushKey(ctx)
	if errors.Is(err, store.ErrNotFound) {
		k, err := webpush.GenerateKey()
		if err != nil {
			return nil, "", err
		}
		raw, err := webpush.KeyBytes(k)
		if err != nil {
			return nil, "", err
		}
		pub, err := webpush.PublicKey(k)
		if err != nil {
			return nil, "", err
		}
		sealed, err := secret.Seal(key, pushPurpose, raw)
		if err != nil {
			return nil, "", err
		}
		if row, err = s.Store.InitPushKey(ctx, store.PushKey{PrivateKeyEnc: sealed, PublicKey: pub}); err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	raw, err := secret.Open(key, pushPurpose, row.PrivateKeyEnc)
	if err != nil {
		return nil, "", errf(CodeFailedPrecondition, "the notification key does not open with VX_SECRET_KEY: %v", err)
	}
	k, err := webpush.ParseKey(raw)
	if err != nil {
		return nil, "", err
	}
	return k, row.PublicKey, nil
}

// PushPublicKey is the key browsers subscribe with (applicationServerKey).
func (s *Service) PushPublicKey(ctx context.Context) (string, error) {
	_, pub, err := s.vapidKey(ctx)
	return pub, err
}

// PushSubscribeInput is a browser's subscription.
type PushSubscribeInput struct {
	Endpoint, P256dh, Auth string
	Device                 string // what the browser is, shown in Settings
}

func pushPeopleOnly(actor Actor) error {
	if actor.Kind != store.KindHuman {
		return errf(CodePermission, "only people turn on notifications; agents read vx inbox and vx wait")
	}
	return nil
}

// SubscribePush turns notifications on for the caller in one browser.
func (s *Service) SubscribePush(ctx context.Context, actor Actor, in PushSubscribeInput) (store.PushSubscription, error) {
	if err := pushPeopleOnly(actor); err != nil {
		return store.PushSubscription{}, err
	}
	sub := webpush.Subscription{Endpoint: strings.TrimSpace(in.Endpoint), P256dh: strings.TrimSpace(in.P256dh), Auth: strings.TrimSpace(in.Auth)}
	if err := webpush.CheckSubscription(sub); err != nil && !s.Push.AllowAnyEndpoint {
		return store.PushSubscription{}, errf(CodeInvalid, "%v; turn notifications off and on again in this browser", err)
	}
	if _, _, err := s.vapidKey(ctx); err != nil {
		return store.PushSubscription{}, err
	}
	have, err := s.Store.PushSubscriptions(ctx, actor.ID)
	if err != nil {
		return store.PushSubscription{}, err
	}
	known := false
	for _, h := range have {
		known = known || h.Endpoint == sub.Endpoint
	}
	if !known && len(have) >= maxPushDevices {
		return store.PushSubscription{}, errf(CodeFailedPrecondition, "notifications are on in %d browsers already; turn them off in one in Settings first", len(have))
	}
	return s.Store.SavePushSubscription(ctx, store.PushSubscription{
		PrincipalID: actor.ID, Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		Device: pushClip(in.Device, 80), CreatedAt: s.Now(),
	})
}

// UnsubscribePush turns notifications off for one of the caller's
// browsers. Turning off one that is not there is not an error.
func (s *Service) UnsubscribePush(ctx context.Context, actor Actor, endpoint string) error {
	if err := pushPeopleOnly(actor); err != nil {
		return err
	}
	_, err := s.Store.DeletePushSubscription(ctx, actor.ID, strings.TrimSpace(endpoint))
	return err
}

// PushSubscriptions lists the browsers the caller gets notifications in.
func (s *Service) PushSubscriptions(ctx context.Context, actor Actor) ([]store.PushSubscription, error) {
	if actor.Kind != store.KindHuman {
		return nil, nil
	}
	return s.Store.PushSubscriptions(ctx, actor.ID)
}

// PushMessage is what the service worker shows (static/sw.js).
type PushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"` // a path on this server
	Tag   string `json:"tag"` // a newer notification with the same tag replaces the older
}

// TestPush sends a test notification to one of the caller's browsers.
func (s *Service) TestPush(ctx context.Context, actor Actor, endpoint string) error {
	if err := pushPeopleOnly(actor); err != nil {
		return err
	}
	subs, err := s.Store.PushSubscriptions(ctx, actor.ID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.Endpoint == strings.TrimSpace(endpoint) {
			sender, err := s.pushSender(ctx)
			if err != nil {
				return err
			}
			err = s.pushTo(ctx, sender, sub, PushMessage{Title: "vx notifications work", Body: "This browser gets questions escalated to you and reviews you may approve.", URL: "/ui/for-you", Tag: "vx-test"})
			if err != nil {
				return errf(CodeFailedPrecondition, "the push service refused the test: %v", err)
			}
			return nil
		}
	}
	return errf(CodeNotFound, "notifications are not on in this browser; turn them on first")
}

func (s *Service) pushSender(ctx context.Context) (*webpush.Sender, error) {
	k, _, err := s.vapidKey(ctx)
	if err != nil {
		return nil, err
	}
	subject := s.publicURL()
	if !strings.HasPrefix(subject, "https://") {
		subject = "https://github.com/TerracodeBA/vx"
	}
	return &webpush.Sender{Key: k, Subject: subject, Client: s.Push.Client, Now: s.Now}, nil
}

// pushTo sends one message to one browser and records the outcome; a
// subscription the push service no longer knows is dropped.
func (s *Service) pushTo(ctx context.Context, sender *webpush.Sender, sub store.PushSubscription, m PushMessage) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	err = sender.Send(ctx, webpush.Subscription{Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth},
		webpush.Message{Payload: payload, TTL: pushTTL, Topic: topic(m.Tag), Urgent: true})
	switch {
	case errors.Is(err, webpush.ErrGone):
		return errors.Join(err, s.Store.DeletePushSubscriptionByID(ctx, sub.ID))
	case err != nil:
		return errors.Join(err, s.Store.MarkPushResult(ctx, sub.ID, s.Now(), pushClip(err.Error(), 300)))
	}
	return s.Store.MarkPushResult(ctx, sub.ID, s.Now(), "")
}

// topic is a push Topic header from a tag: up to 32 URL-safe characters.
func topic(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() == 32 {
			break
		}
	}
	return b.String()
}

// pushItem is something a person may be notified of.
type pushItem struct {
	key string // push_sent item
	at  time.Time
	msg PushMessage
}

// pushItems lists what to notify a person of: questions escalated to them
// or their agents (or asked of them), and reviews they may decide.
func (s *Service) pushItems(ctx context.Context, actor Actor) ([]pushItem, error) {
	in, err := s.GetInbox(ctx, actor)
	if err != nil {
		return nil, err
	}
	var out []pushItem
	seen := map[uuid.UUID]bool{}
	question := func(n Note) {
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		who := n.Author.Ref()
		title := "Question from " + who
		if n.State == NoteEscalated {
			title = "Escalated: question from " + who
		}
		out = append(out, pushItem{key: "question:" + n.ID.String(), at: noteTime(n), msg: PushMessage{
			Title: title, Body: pushClip(n.RepoName+" / "+n.EffortSlug+": "+n.Body, 180),
			URL: "/ui/for-you#n-" + n.ID.String(), Tag: "q-" + n.ID.String()[:8],
		}})
	}
	for _, n := range in.Escalated {
		question(n)
	}
	for _, n := range in.Questions {
		if n.State == NoteEscalated || (n.To != nil && n.To.ID == actor.ID) {
			question(n)
		}
	}
	reviews, err := s.ListReviews(ctx, actor, ReviewQuery{})
	if err != nil {
		return nil, err
	}
	for _, rv := range reviews {
		if !rv.CanDecide {
			continue
		}
		title, _, _ := strings.Cut(rv.Intent, "\n")
		out = append(out, pushItem{key: "review:" + rv.ID.String(), at: rv.CreatedAt, msg: PushMessage{
			Title: "Review waiting: " + pushClip(title, 80), Body: pushClip(rv.RepoName+" / "+rv.EffortSlug+" by "+rv.Author.Ref()+": "+strings.Join(rv.Paths, ", "), 180),
			URL: "/ui/for-you#r-" + rv.ID.String(), Tag: "r-" + rv.ID.String()[:8],
		}})
	}
	return out, nil
}

// NotifyPush runs one round of the notifier: for each person with
// notifications on, it pushes what is new since they turned them on. It
// returns how many notifications went out.
func (s *Service) NotifyPush(ctx context.Context) (int, error) {
	ids, err := s.Store.PushRecipients(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	sender, err := s.pushSender(ctx)
	if err != nil {
		return 0, err
	}
	now := s.Now()
	sent := 0
	var errs []error
	for _, id := range ids {
		n, err := s.notifyPerson(ctx, sender, id, now)
		sent += n
		if err != nil {
			errs = append(errs, fmt.Errorf("principal %s: %w", id, err))
		}
	}
	if err := s.Store.PrunePushSent(ctx, now.Add(-pushForget)); err != nil {
		errs = append(errs, err)
	}
	return sent, errors.Join(errs...)
}

func (s *Service) notifyPerson(ctx context.Context, sender *webpush.Sender, id uuid.UUID, now time.Time) (int, error) {
	p, err := s.Store.PrincipalByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if p.Kind != store.KindHuman {
		return 0, nil
	}
	actor, err := s.ResolveActor(ctx, p)
	if err != nil {
		return 0, err
	}
	subs, err := s.Store.PushSubscriptions(ctx, id)
	if err != nil || len(subs) == 0 {
		return 0, err
	}
	since := subs[0].CreatedAt
	for _, sub := range subs {
		if sub.CreatedAt.Before(since) {
			since = sub.CreatedAt
		}
	}
	if w := now.Add(-pushWindow); since.Before(w) {
		since = w
	}
	items, err := s.pushItems(ctx, actor)
	if err != nil {
		return 0, err
	}
	var fresh []pushItem
	for _, it := range items {
		if it.at.Before(since) || it.at.After(now) {
			continue
		}
		first, err := s.Store.ClaimPush(ctx, id, it.key, now)
		if err != nil {
			return 0, err
		}
		if first {
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	msgs := make([]PushMessage, 0, len(fresh))
	if len(fresh) > pushBatch {
		msgs = append(msgs, PushMessage{Title: fmt.Sprintf("%d things wait for you in vx", len(fresh)),
			Body: "Questions and reviews: open For you to answer or approve.", URL: "/ui/for-you", Tag: "vx-summary"})
	} else {
		for _, it := range fresh {
			msgs = append(msgs, it.msg)
		}
	}
	sent := 0
	var errs []error
	for _, sub := range subs {
		for _, m := range msgs {
			if err := s.pushTo(ctx, sender, sub, m); err != nil {
				if errors.Is(err, webpush.ErrGone) {
					break
				}
				errs = append(errs, err)
				continue
			}
			sent++
		}
	}
	return sent, errors.Join(errs...)
}

// RunPushNotifier runs NotifyPush every PushSettings.Every until ctx ends.
// Without VX_SECRET_KEY there is nothing to sign with, and it returns.
func (s *Service) RunPushNotifier(ctx context.Context, log *slog.Logger) {
	if !s.PushAvailable() {
		return
	}
	every := s.Push.Every
	if every <= 0 {
		every = DefaultPushEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		run, cancel := context.WithTimeout(ctx, every)
		if n, err := s.NotifyPush(run); err != nil && ctx.Err() == nil {
			log.Warn("push notifications", "sent", n, "err", err)
		}
		cancel()
	}
}

// pushClip shortens s to at most n runes, with an ellipsis when cut.
func pushClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
