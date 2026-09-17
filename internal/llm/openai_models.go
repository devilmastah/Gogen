package llm

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"gogen/internal/config"
	"gogen/internal/modelinfo"

	"github.com/openai/openai-go/v3"
)

func (p *OpenAIProvider) listModels(ctx context.Context) ([]openai.Model, error) {
	return p.listModelsInternal(ctx, true)
}

// listModelsProbe is listModels for the brief, best-effort catalog lookup in
// ModelContextLimit: a failed fetch does NOT arm the shared
// modelsFetchFailedAt backoff. That lookup runs under
// modelsLimitLookupTimeout (1.5s), which routinely cuts off a slow-but-alive
// remote catalog; treating the timeout as a dead-endpoint signal would make
// clientForModel skip catalog discovery for modelsFetchBackoff and route
// every model through the fallback/owner inference. Only full-budget catalog
// fetches (ListModels, clientForModel's discovery) arm the backoff.
func (p *OpenAIProvider) listModelsProbe(ctx context.Context) ([]openai.Model, error) {
	return p.listModelsInternal(ctx, false)
}

// listModelsInternal fetches the merged catalog with a single-flight guard
// and a modelsCatalogTimeout bound. armBackoff selects whether a failed fetch
// arms the shared failure backoff (modelsFetchFailedAt): callers whose budget
// is shorter than a full catalog fetch (the ModelContextLimit probe) pass
// false so their brief timeout cannot degrade routing.
func (p *OpenAIProvider) listModelsInternal(ctx context.Context, armBackoff bool) ([]openai.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
	defer cancel()

	p.modelsMu.RLock()
	if models, ok := p.cachedModelsLocked(); ok {
		out := append([]openai.Model(nil), models...)
		p.modelsMu.RUnlock()
		return out, nil
	}
	if f := p.modelsFetch; f != nil {
		p.modelsMu.RUnlock()
		return waitModelsFetch(ctx, f)
	}
	p.modelsMu.RUnlock()

	p.modelsMu.Lock()
	if models, ok := p.cachedModelsLocked(); ok {
		out := append([]openai.Model(nil), models...)
		p.modelsMu.Unlock()
		return out, nil
	}
	if f := p.modelsFetch; f != nil {
		p.modelsMu.Unlock()
		return waitModelsFetch(ctx, f)
	}
	f := &modelsFetch{done: make(chan struct{})}
	p.modelsFetch = f
	gen := p.profilesGen
	p.modelsMu.Unlock()

	models, profileFor, perProfile, err := p.fetchModelsWithProfiles(ctx, armBackoff)

	p.modelsMu.Lock()
	f.models, f.err = models, err
	if err == nil {
		if p.profilesGen == gen {
			p.modelsCache = models
			p.modelsCachedAt = time.Now()
			p.modelProfile = profileFor
			// Merge, don't replace: clientForModel caches fallback entries for
			// models absent from the catalog (unknown/custom models resolved
			// via models.dev or the primary client). A wholesale replacement
			// would drop those entries and force a fresh lookup + inference on
			// every request after each catalog refresh. applyRouting keeps
			// entries for models the round does not cover (they only point at
			// the endpoint that used to serve it, so this is a minor risk).
			p.applyRouting(perProfile, models, profileFor)
			// Publish the effective owners to the shared record so sibling
			// providers (fresh sessions, subagents) route a model to its owner
			// even while the owner's catalog is unreachable.
			if p.ownerRegistry != nil {
				for id, owner := range p.modelOwner {
					p.ownerRegistry.Record(id, owner)
				}
			}
		}
		// Stale fetch (SetProfiles swapped the endpoint set while this
		// catalog fetch was in flight): the result came from the OLD
		// endpoints and is NOT cached — concurrent waiters get the snapshot
		// once, and the next call re-fetches the new endpoint set.
		p.modelsFetchFailedAt = time.Time{}
	} else if armBackoff {
		// Record the failure so clientForModel's backoff gate skips re-probing
		// a dead catalog on every chat request. A brief probe
		// (listModelsProbe) leaves the marker untouched: its short budget is
		// not evidence the endpoint is dead.
		p.modelsFetchFailedAt = time.Now()
	}
	p.modelsFetch = nil
	close(f.done)
	p.modelsMu.Unlock()
	if err != nil {
		return nil, err
	}
	return append([]openai.Model(nil), models...), nil
}

// fetchEndpointModels returns one endpoint's catalog, sharing the download with
// every other provider in the process that talks to the same endpoint (see
// EndpointCatalog). Providers built without a shared catalog (tests, direct
// construction) fetch directly, exactly as before.
func (p *OpenAIProvider) fetchEndpointModels(ctx context.Context, key string, armBackoff bool, fetch func(context.Context) ([]openai.Model, error)) ([]openai.Model, error) {
	p.modelsMu.RLock()
	c := p.endpointCatalog
	p.modelsMu.RUnlock()
	if c == nil {
		return fetch(ctx)
	}
	return c.Models(ctx, key, armBackoff, fetch)
}

// catalogFetchOnBackoff reports whether a failed catalog fetch is still in
// its backoff window, so a dead OpenCode endpoint is not re-probed on every
// chat request. A fetch that failed moments ago costs one bounded attempt
// per modelsFetchBackoff window instead of one per request.
func (p *OpenAIProvider) catalogFetchOnBackoff() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return !p.modelsFetchFailedAt.IsZero() &&
		time.Since(p.modelsFetchFailedAt) < modelsFetchBackoff
}

func waitModelsFetch(ctx context.Context, f *modelsFetch) ([]openai.Model, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return append([]openai.Model(nil), f.models...), nil
	}
}

// profileFetchResult is one profile's catalog outcome for a fetch round:
// its name, the model → stream-client routing it contributed (empty when
// the query failed), and the query error. listModels applies the
// sticky-ownership rule (applyRouting) over the per-profile results — the
// merged winner alone cannot tell a downed owner's model from a same-ID
// collision on a surviving endpoint.
type profileFetchResult struct {
	name    string
	routing map[string]*openai.Client
	err     error
}

// fetchModelsWithProfiles loads the merged catalog from every registered
// provider endpoint: each profile's catalog is queried in parallel (an
// OpenCode profile queries its zen and go endpoints in parallel with
// Go-over-Zen precedence, exactly like the single-endpoint shape), then the
// per-profile lists are merged in PROFILE ORDER — the first profile wins on
// duplicate model IDs (the default profile first), so what /models shows
// agrees with the routing for models with no prior owner. The returned
// profileFor map records each model's winning profile (name + base URL) for
// picker grouping and per-endpoint models.dev / props resolution, and the
// perProfile slice carries every profile's own outcome (including failures)
// for the sticky-ownership merge.
func (p *OpenAIProvider) fetchModelsWithProfiles(ctx context.Context, armBackoff bool) ([]openai.Model, map[string]modelProfileInfo, []profileFetchResult, error) {
	type result struct {
		models  []openai.Model
		routing map[string]*openai.Client
		err     error
	}
	// query lists one endpoint. The download itself is shared process-wide
	// (see EndpointCatalog): every session otherwise re-fetched the same
	// endpoint's /v1/models. Only the model → stream-client routing is built
	// here, because client pointers are per provider.
	query := func(qctx context.Context, key string, catalog, stream *openai.Client) result {
		if catalog == nil {
			catalog = stream
		}
		models, err := p.fetchEndpointModels(qctx, key, armBackoff, func(fctx context.Context) ([]openai.Model, error) {
			var listed []openai.Model
			pager := catalog.Models.ListAutoPaging(fctx)
			for pager.Next() {
				if fctx.Err() != nil {
					return listed, fctx.Err()
				}
				listed = append(listed, pager.Current())
			}
			return listed, pager.Err()
		})
		routing := make(map[string]*openai.Client, len(models))
		if stream != nil {
			for i := range models {
				routing[models[i].ID] = stream
			}
		}
		return result{models: models, routing: routing, err: err}
	}

	// queryProfile fetches one profile's endpoint set. An OpenCode profile
	// queries zen + go in parallel; Go takes precedence over Zen for models
	// listed on both (a Go-subscription model must never be sent to the Zen
	// endpoint, which rejects it as unsupported).
	queryProfile := func(qctx context.Context, prof *providerProfile) result {
		if prof.zenStream == nil || prof.goStream == nil {
			return query(qctx, CatalogKey(prof.baseURL, prof.apiKey), prof.catalog, prof.stream)
		}
		var zenRes, goRes result
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			zenRes = query(qctx, CatalogKey(openCodeZenBaseURL, prof.apiKey), prof.zenCatalog, prof.zenStream)
		}()
		go func() {
			defer wg.Done()
			goRes = query(qctx, CatalogKey(openCodeGoBaseURL, prof.apiKey), prof.goCatalog, prof.goStream)
		}()
		// Join both queries before reading their results. Each query observes
		// qctx cancellation and its catalog client carries its own timeout,
		// so this wait is bounded even when one endpoint hangs. Waiting (rather
		// than returning early on qctx.Done) also keeps whatever the surviving
		// twin already listed: a hung Zen or Go endpoint must not discard the
		// other's completed catalog, matching the per-profile merge rule.
		wg.Wait()

		routing := make(map[string]*openai.Client, len(zenRes.routing)+len(goRes.routing))
		models := make([]openai.Model, 0, len(zenRes.models)+len(goRes.models))
		seen := make(map[string]struct{}, len(goRes.models))
		for _, m := range goRes.models {
			models = append(models, m)
			seen[m.ID] = struct{}{}
		}
		for _, m := range zenRes.models {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			models = append(models, m)
		}
		for id, c := range goRes.routing {
			routing[id] = c
		}
		for id, c := range zenRes.routing {
			if _, goWins := routing[id]; goWins {
				continue
			}
			routing[id] = c
		}
		var errs []error
		if zenRes.err != nil {
			errs = append(errs, zenRes.err)
		}
		if goRes.err != nil {
			errs = append(errs, goRes.err)
		}
		if len(models) == 0 && len(errs) > 0 {
			return result{err: errors.Join(errs...)}
		}
		return result{models: models, routing: routing}
	}

	profiles := p.resolvedProfiles()
	results := make([]result, len(profiles))
	var wg sync.WaitGroup
	for i, prof := range profiles {
		wg.Add(1)
		go func(i int, prof *providerProfile) {
			defer wg.Done()
			// Per-profile deadline: a hung endpoint (offline host that never
			// refuses the connection) must not hold the picker — or the
			// first chat request — hostage for the whole fetch budget while
			// the other providers already answered. The merge below keeps
			// every catalog that completed within this window.
			qctx, cancel := context.WithTimeout(ctx, profileCatalogTimeout)
			defer cancel()
			results[i] = queryProfile(qctx, prof)
		}(i, prof)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// Wait for every profile query to settle — including after the fetch
	// budget expires: an offline or hanging provider must not discard the
	// catalogs that already succeeded (the early ctx.Done() exit here used
	// to empty the whole model picker over a single dead endpoint). The
	// in-flight queries observe ctx cancellation and each catalog HTTP
	// client carries its own timeout, so this wait is bounded; the merge
	// below keeps every profile that completed and only reports an error
	// when ALL profiles failed.
	<-done

	models := make([]openai.Model, 0)
	profileFor := make(map[string]modelProfileInfo)
	seen := make(map[string]struct{})
	var errs []error
	perProfile := make([]profileFetchResult, 0, len(profiles))
	for i, prof := range profiles {
		res := results[i]
		perProfile = append(perProfile, profileFetchResult{name: prof.name, routing: res.routing, err: res.err})
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		// Add this profile's models in profile order: the FIRST profile to
		// list a model ID wins the merged list (the default profile first).
		for _, m := range res.models {
			if _, dup := seen[m.ID]; dup {
				continue // earlier profile wins
			}
			seen[m.ID] = struct{}{}
			models = append(models, m)
			profileFor[m.ID] = modelProfileInfo{name: prof.name, baseURL: prof.baseURL}
		}
	}
	if len(models) == 0 && len(errs) > 0 {
		return nil, nil, perProfile, errors.Join(errs...)
	}
	if len(errs) > 0 {
		// Partial failure: serve the catalogs that succeeded and say which
		// providers dropped out — the picker must not go empty because one
		// endpoint is offline.
		log.Printf("model catalog: %d of %d provider(s) failed, serving %d models: %v",
			len(errs), len(profiles), len(models), errors.Join(errs...))
	}
	return models, profileFor, perProfile, nil
}

// applyRouting rebuilds the model → client routing from one fetch round's
// per-profile results under the sticky-ownership rule:
//
//   - A model's owner is the earliest profile that lists it (profile-order
//     precedence, as before) — UNLESS the model already has an owner that is
//     up this round and still lists it: the owner stays. A model the user
//     runs on one endpoint is not re-homed to an earlier-listing endpoint
//     mid-session.
//   - When the owner is down for the round, the model fails over to a
//     surviving lister only if that lister was observed listing the model in
//     a round where the owner was up (modelCoListed — a genuine co-listed
//     endpoint). A same-ID model on an endpoint that never co-listed is a
//     collision, not ownership: the downed owner is kept, and
//     clientForModel's owner fallback resolves its client.
//   - When the owner is up but no longer lists the model, the model moved:
//     the earliest surviving lister takes over.
//
// Entries for models no surviving profile lists linger (as before): they
// only point at the endpoint that used to serve them. Caller holds modelsMu
// (the shared-registry publish happens in listModels, which may then take
// the registry lock — see OwnerRegistry's lock-ordering note).
func (p *OpenAIProvider) applyRouting(perProfile []profileFetchResult, models []openai.Model, profileFor map[string]modelProfileInfo) {
	if p.modelClient == nil {
		p.modelClient = make(map[string]*openai.Client)
	}
	if p.modelOwner == nil {
		p.modelOwner = make(map[string]string)
	}
	if p.modelCoListed == nil {
		p.modelCoListed = make(map[string]map[string]struct{})
	}
	byName := make(map[string]*profileFetchResult, len(perProfile))
	for i := range perProfile {
		byName[perProfile[i].name] = &perProfile[i]
	}
	for _, m := range models {
		id := m.ID
		if id == "" {
			continue
		}
		winner := profileFor[id].name
		old, known := p.modelOwner[id]
		owner := winner
		if known && old != winner {
			oldRes := byName[old]
			switch {
			case oldRes != nil && oldRes.err == nil:
				if _, still := oldRes.routing[id]; still {
					owner = old // sticky: the owner is up and still serves it
				}
				// else: the owner is up but no longer lists the model — it
				// moved; the earliest surviving lister takes over.
			case coListedKnown(p.modelCoListed[id], winner):
				owner = winner // failover to a known co-listed endpoint
			default:
				owner = old // sticky: the owner is down, no co-list known
			}
		}
		p.modelOwner[id] = owner
		ownerRes := byName[owner]
		if ownerRes == nil || ownerRes.err != nil {
			// The owner is down for this round: keep any existing entry (it
			// points at the owner's last client) and the last co-list
			// observation.
			continue
		}
		if c, ok := ownerRes.routing[id]; ok {
			p.modelClient[id] = c
		}
		// The owner is up: every up lister of this model is now a known
		// co-list — a legitimate failover target if the owner later goes
		// down.
		co := make(map[string]struct{})
		for i := range perProfile {
			r := &perProfile[i]
			if r.err != nil {
				continue
			}
			if _, ok := r.routing[id]; ok {
				co[r.name] = struct{}{}
			}
		}
		p.modelCoListed[id] = co
	}
}

// coListedKnown reports whether name is in the co-list set for a model.
func coListedKnown(co map[string]struct{}, name string) bool {
	_, ok := co[name]
	return ok
}

// resolvedProfiles returns the registered endpoint set to query (nil for
// direct-constructed providers with no profiles; the callers treat an empty
// set as "nothing to fetch").
func (p *OpenAIProvider) resolvedProfiles() []*providerProfile {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.profiles
}

func (p *OpenAIProvider) ModelContextLimit(ctx context.Context) (int, error) {
	// Resolution order: warm in-memory cache → models.dev (disk-cached, no
	// network) → brief /v1/models probe → 128k default. Network catalog
	// uses a short timeout so a hung remote host cannot stall restore or
	// context refresh; local servers still win when they answer quickly
	// with n_ctx etc.
	ctx, cancel := context.WithTimeout(ctx, modelsLimitLookupTimeout)
	defer cancel()

	// 1. Warm in-memory cache: parse provider JSON with no I/O.
	p.modelsMu.RLock()
	cached, cacheOK := p.cachedModelsLocked()
	if cacheOK {
		cached = append([]openai.Model(nil), cached...)
	}
	p.modelsMu.RUnlock()
	if cacheOK {
		if lim, ok := p.contextLimitFromModels(cached); ok {
			return lim, nil
		}
	}

	// 2. models.dev registry: disk-cached, resolves instantly with no network.
	modelName := p.currentModel()
	if limit, _, _, _ := p.lookupModelsDevInfo(modelName); limit > 0 {
		return limit, nil
	}

	// 3. Brief /v1/models probe (fallback when models.dev has no entry).
	//    Local LLMs typically answer in << 1s; a hung remote host is bounded
	//    by the outer modelsLimitLookupTimeout context. The probe uses
	//    listModelsProbe so a timeout from that brief budget never arms the
	//    shared catalog backoff (which would degrade clientForModel routing
	//    for a slow-but-alive remote catalog).
	models, _ := p.listModelsProbe(ctx)
	if lim, ok := p.contextLimitFromModels(models); ok {
		return lim, nil
	}
	// 4. Fallback — return the default.
	return config.DefaultContextLimit, nil
}

// contextLimitFromModels applies sole-model auto-select and provider JSON
// context fields. ok is false when no positive limit was found. Sole-model
// auto-select only applies to the single default endpoint: with additional
// registered profiles the aggregate list is a merge of several catalogs and
// "sole model" is not meaningful.
func (p *OpenAIProvider) contextLimitFromModels(models []openai.Model) (int, bool) {
	if len(models) == 1 && !p.hasMultipleProfiles() {
		sole := models[0]
		p.modelsMu.Lock()
		if sole.ID != "" {
			p.model = sole.ID
		}
		p.modelsMu.Unlock()
		if limit := parseContextLimitFromJSON(sole.RawJSON()); limit > 0 {
			return limit, true
		}
	}
	modelName := p.currentModel()
	for _, model := range models {
		if model.ID != modelName {
			continue
		}
		if limit := parseContextLimitFromJSON(model.RawJSON()); limit > 0 {
			return limit, true
		}
	}
	return 0, false
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.listModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(models))
	current := p.currentModel()
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		limit, cost, efforts, desc := p.resolveContextLimit(m.RawJSON(), m.ID)
		info := ModelInfo{
			ID:               m.ID,
			ContextLimit:     limit,
			Current:          m.ID == current,
			Provider:         p.profileFor(m.ID),
			Description:      desc,
			ReasoningEfforts: efforts,
		}
		if m.ID == current && cost != nil {
			info.InputPricePer1M = cost.Input
			info.OutputPricePer1M = cost.Output
			info.CachedPricePer1M = cost.CacheRead
		}
		out = append(out, info)
	}
	return out, nil
}

// modelsDevURLsFor returns the models.dev base URLs to try for modelID: the
// owning profile's URLs first (so a model listed on a second endpoint
// resolves against its own registry entry), falling back to the default
// profile's URLs when the catalog has not been fetched.
func (p *OpenAIProvider) modelsDevURLsFor(modelID string) []string {
	p.modelsMu.RLock()
	info := p.modelProfile[modelID]
	p.modelsMu.RUnlock()
	if info.baseURL != "" {
		return modelsDevURLsForBaseURL(info.baseURL)
	}
	return modelsDevURLsForBaseURL(p.defaultBaseURL())
}

// modelsDevURLsForBaseURL is the shared URL derivation for one endpoint: the
// base URL itself, plus the OpenCode zen/go endpoints when it is an OpenCode
// URL. Deduplicated.
func modelsDevURLsForBaseURL(baseURL string) []string {
	urls := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(u string) {
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	add(baseURL)
	if isOpencodeURL(baseURL) {
		add(openCodeZenBaseURL)
		add(openCodeGoBaseURL)
	}
	return urls
}

// lookupModelsDevInfo queries the models.dev registry by base URL + model ID
// for context limit, pricing, accepted reasoning-effort values, and the model
// description in a single pass. OpenCode dual endpoints are both tried so zen
// and go models resolve. Only models.dev entries with a usable limit or cost
// are returned; the loop keeps trying the remaining URLs otherwise, matching
// the previous behavior.
func (p *OpenAIProvider) lookupModelsDevInfo(modelID string) (int, *modelinfo.Cost, []string, string) {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return 0, nil, nil, ""
	}
	for _, u := range p.modelsDevURLsFor(modelID) {
		lim, cost, efforts, desc, err := p.modelInfo.Resolve(u, modelID)
		if err != nil {
			continue
		}
		var c *modelinfo.Cost
		if cost != nil && (cost.Input > 0 || cost.Output > 0) {
			c = cost
		}
		if lim.Context > 0 {
			return lim.Context, c, efforts, desc
		}
		if c != nil {
			return 0, c, efforts, desc
		}
	}
	return 0, nil, nil, ""
}

// resolveContextLimit resolves context limit, pricing, accepted
// reasoning-effort values, and the model description. Provider JSON is tried
// first for the limit, but models.dev is always consulted for cost, efforts,
// and description since provider JSON never includes them.
func (p *OpenAIProvider) resolveContextLimit(rawJSON, modelID string) (int, *modelinfo.Cost, []string, string) {
	// Always look up models.dev for pricing/efforts/description (provider JSON
	// never has them).
	var cost *modelinfo.Cost
	var efforts []string
	var desc string
	devLimit, devCost, devEfforts, devDesc := p.lookupModelsDevInfo(modelID)
	if devCost != nil {
		cost = devCost
	}
	if len(devEfforts) > 0 {
		efforts = devEfforts
	}
	if devDesc != "" {
		desc = devDesc
	}
	// Prefer provider JSON limit, then models.dev, then default.
	if limit := parseContextLimitFromJSON(rawJSON); limit > 0 {
		return limit, cost, efforts, desc
	}
	if devLimit > 0 {
		return devLimit, cost, efforts, desc
	}
	return config.DefaultContextLimit, cost, efforts, desc
}
