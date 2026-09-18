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

// catalogModel is one catalog entry with its owning profile tag. The merged
// list may contain the same model ID under different providers (providers
// are separate instances — see fetchModelsWithProfiles), so the tag travels
// with the entry: an ID-keyed map cannot express which provider's copy an
// entry is.
type catalogModel struct {
	model   openai.Model
	profile string
}

func (p *OpenAIProvider) listModels(ctx context.Context) ([]catalogModel, error) {
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
func (p *OpenAIProvider) listModelsProbe(ctx context.Context) ([]catalogModel, error) {
	return p.listModelsInternal(ctx, false)
}

// listModelsInternal fetches the merged catalog with a single-flight guard
// and a modelsCatalogTimeout bound. armBackoff selects whether a failed fetch
// arms the shared failure backoff (modelsFetchFailedAt): callers whose budget
// is shorter than a full catalog fetch (the ModelContextLimit probe) pass
// false so their brief timeout cannot degrade routing.
func (p *OpenAIProvider) listModelsInternal(ctx context.Context, armBackoff bool) ([]catalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
	defer cancel()

	p.modelsMu.RLock()
	if models, ok := p.cachedModelsLocked(); ok {
		out := append([]catalogModel(nil), models...)
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
		out := append([]catalogModel(nil), models...)
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

	models, perProfile, err := p.fetchModelsWithProfiles(ctx, armBackoff)

	p.modelsMu.Lock()
	f.models, f.err = models, err
	if err == nil {
		if p.profilesGen == gen {
			p.modelsCache = models
			p.modelsCachedAt = time.Now()
			// Rebuild the per-model profile record from the tagged entries:
			// it serves the models.dev /props lookups that need the OWNING
			// profile's base URL (a bare ID can no longer name it — the same
			// ID may sit under several providers).
			profileBaseURL := make(map[string]string, len(p.profiles))
			for _, prof := range p.profiles {
				profileBaseURL[prof.name] = prof.baseURL
			}
			p.modelProfile = make(map[ModelRef]modelProfileInfo, len(models))
			for _, cm := range models {
				p.modelProfile[ModelRef{Profile: cm.profile, ID: cm.model.ID}] = modelProfileInfo{name: cm.profile, baseURL: profileBaseURL[cm.profile]}
			}
			// applyRouting rebuilds the OpenCode twin routing (model → the
			// zen/go client that serves it). Non-OpenCode profiles need no
			// routing entry: clientForModel uses their stream client
			// directly.
			p.applyRouting(perProfile)
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
	return append([]catalogModel(nil), models...), nil
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

func waitModelsFetch(ctx context.Context, f *modelsFetch) ([]catalogModel, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return append([]catalogModel(nil), f.models...), nil
	}
}

// profileFetchResult is one profile's catalog outcome for a fetch round:
// its name, the model → stream-client routing it contributed (empty when
// the query failed), and the query error. applyRouting consumes the
// per-profile results to rebuild the OpenCode twin routing.
type profileFetchResult struct {
	name    string
	routing map[string]*openai.Client
	err     error
}

// fetchModelsWithProfiles loads the merged catalog from every registered
// provider endpoint: each profile's catalog is queried in parallel (an
// OpenCode profile queries its zen and go endpoints in parallel with
// Go-over-Zen precedence — the ONLY cross-endpoint merge, since the two
// gateways are one logical provider), then the per-profile lists are
// concatenated in PROFILE ORDER (the default profile first) with NO
// cross-provider dedupe: providers are separate instances, so the same
// model ID may appear once per provider that lists it, each entry tagged
// with its owning profile. The picker shows every entry under its provider
// group, and routing is per (profile, ID) — a model the user picks under
// one provider never routes to another. The returned perProfile slice
// carries every profile's own outcome (including failures) for the twin
// routing rebuild (applyRouting).
func (p *OpenAIProvider) fetchModelsWithProfiles(ctx context.Context, armBackoff bool) ([]catalogModel, []profileFetchResult, error) {
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

	models := make([]catalogModel, 0)
	var errs []error
	perProfile := make([]profileFetchResult, 0, len(profiles))
	for i, prof := range profiles {
		res := results[i]
		perProfile = append(perProfile, profileFetchResult{name: prof.name, routing: res.routing, err: res.err})
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		// Concatenate in profile order (default first) with NO cross-provider
		// dedupe: a model ID listed by several providers appears once per
		// provider, tagged with its owner. (The OpenCode profile's own
		// zen/go dedupe already happened in queryProfile — that twin is one
		// logical provider.)
		for _, m := range res.models {
			models = append(models, catalogModel{model: m, profile: prof.name})
		}
	}
	if len(models) == 0 && len(errs) > 0 {
		return nil, perProfile, errors.Join(errs...)
	}
	if len(errs) > 0 {
		// Partial failure: serve the catalogs that succeeded and say which
		// providers dropped out — the picker must not go empty because one
		// endpoint is offline.
		log.Printf("model catalog: %d of %d provider(s) failed, serving %d models: %v",
			len(errs), len(profiles), len(models), errors.Join(errs...))
	}
	return models, perProfile, nil
}

// applyRouting rebuilds the OpenCode twin routing from one fetch round's
// per-profile results: for each OpenCode profile, every model its merged
// zen/go catalog listed maps to the twin that serves it (queryProfile
// already applied the Go-over-Zen precedence). Non-OpenCode profiles need
// no entry — clientForModel uses their stream client directly. The map is
// rebuilt from scratch each round: entries for models the round no longer
// lists are stale, and the models.dev fallback (inferOpenCodeEndpoint)
// covers the gap until the next fetch. Caller holds modelsMu (the
// shared-registry publish happens in listModels, which may then take the
// registry lock — see OwnerRegistry's lock-ordering note).
func (p *OpenAIProvider) applyRouting(perProfile []profileFetchResult) {
	p.modelClient = make(map[ModelRef]*openai.Client)
	byName := make(map[string]*profileFetchResult, len(perProfile))
	for i := range perProfile {
		byName[perProfile[i].name] = &perProfile[i]
	}
	for _, prof := range p.profiles {
		if prof.zenStream == nil || prof.goStream == nil {
			continue // not an OpenCode profile: clientForModel routes directly
		}
		r := byName[prof.name]
		if r == nil || r.err != nil {
			continue // the profile's catalog is down this round
		}
		for id, c := range r.routing {
			p.modelClient[ModelRef{Profile: prof.name, ID: id}] = c
		}
	}
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
		cached = append([]catalogModel(nil), cached...)
	}
	p.modelsMu.RUnlock()
	if cacheOK {
		if lim, ok := p.contextLimitFromModels(cached); ok {
			return lim, nil
		}
	}

	// 2. models.dev registry: disk-cached, resolves instantly with no network.
	if limit, _, _, _ := p.lookupModelsDevInfo(p.CurrentModelRef()); limit > 0 {
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
func (p *OpenAIProvider) contextLimitFromModels(models []catalogModel) (int, bool) {
	if len(models) == 1 && !p.hasMultipleProfiles() {
		sole := models[0].model
		p.modelsMu.Lock()
		if sole.ID != "" {
			p.model = sole.ID
			// Sole-model auto-select only runs with the single default
			// profile, so the owner is the default profile ("" — the
			// routing key and the wire ID must change together).
			p.currentModelProfile = ""
		}
		p.modelsMu.Unlock()
		if limit := parseContextLimitFromJSON(sole.RawJSON()); limit > 0 {
			return limit, true
		}
	}
	// Match the CURRENT routing key (owning profile + ID), not the bare ID:
	// model IDs are not unique across registered providers, and the first
	// catalog entry with a matching ID may belong to a different endpoint
	// (e.g. a model an OpenCode catalog also lists) whose JSON reports a
	// different n_ctx — the wrong context window for this session.
	ref := p.CurrentModelRef().Normalized()
	for _, cm := range models {
		entry := ModelRef{Profile: cm.profile, ID: cm.model.ID}
		if entry.Normalized() != ref {
			continue
		}
		if limit := parseContextLimitFromJSON(cm.model.RawJSON()); limit > 0 {
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
	for _, cm := range models {
		m := cm.model
		if m.ID == "" {
			continue
		}
		ref := ModelRef{Profile: cm.profile, ID: m.ID}
		limit, cost, efforts, desc := p.resolveContextLimit(m.RawJSON(), ref)
		info := ModelInfo{
			ID:               m.ID,
			ContextLimit:     limit,
			Current:          m.ID == current,
			Provider:         ref.ProfileName(),
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

// modelsDevURLsFor returns the models.dev base URLs to try for the model
// reference: the owning profile's URLs (so a model listed on a second
// endpoint resolves against its own registry entry). A ref with no profile
// (bare-ID callers: pricing, context-limit lookups) resolves the owner from
// the cached catalog when possible, falling back to the default profile's
// URLs when the catalog has not been fetched.
func (p *OpenAIProvider) modelsDevURLsFor(ref ModelRef) []string {
	p.modelsMu.RLock()
	info := p.modelProfile[ref.Normalized()]
	if info.baseURL == "" && ref.Profile == "" {
		// Bare ID: find the cached catalog entry that owns it.
		for _, cm := range p.modelsCache {
			if cm.model.ID == ref.ID {
				info = p.modelProfile[ModelRef{Profile: cm.profile, ID: ref.ID}]
				break
			}
		}
	}
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

// refForModelID resolves the ModelRef routing key for a bare model ID: the
// owning profile from the cached catalog when the ID is listed (the first
// cached entry wins — profile order, the default profile first), else a
// default-profile ref. Used by bare-ID callers (the /props effort probe)
// that must key per-model caches by the owning profile, not the bare ID.
func (p *OpenAIProvider) refForModelID(modelID string) ModelRef {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	for _, cm := range p.modelsCache {
		if cm.model.ID == modelID {
			return ModelRef{Profile: cm.profile, ID: modelID}
		}
	}
	return ModelRef{ID: modelID}
}

// lookupModelsDevInfo queries the models.dev registry by base URL + model ID
// for context limit, pricing, accepted reasoning-effort values, and the model
// description in a single pass. OpenCode dual endpoints are both tried so zen
// and go models resolve. Only models.dev entries with a usable limit or cost
// are returned; the loop keeps trying the remaining URLs otherwise, matching
// the previous behavior.
func (p *OpenAIProvider) lookupModelsDevInfo(ref ModelRef) (int, *modelinfo.Cost, []string, string) {
	if p == nil || p.modelInfo == nil || !ref.Valid() {
		return 0, nil, nil, ""
	}
	for _, u := range p.modelsDevURLsFor(ref) {
		lim, cost, efforts, desc, err := p.modelInfo.Resolve(u, ref.ID)
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
func (p *OpenAIProvider) resolveContextLimit(rawJSON string, ref ModelRef) (int, *modelinfo.Cost, []string, string) {
	// Always look up models.dev for pricing/efforts/description (provider JSON
	// never has them).
	var cost *modelinfo.Cost
	var efforts []string
	var desc string
	devLimit, devCost, devEfforts, devDesc := p.lookupModelsDevInfo(ref)
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
