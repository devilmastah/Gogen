package llm

// DefaultProfileName is the canonical name of the implicit provider profile
// built from the legacy config fields (profiles[0]).
const DefaultProfileName = "default"

// ModelRef identifies a model for ROUTING: the registered provider profile
// that serves it (Profile) plus the bare model ID the endpoint expects on
// the wire (ID). Model IDs are NOT globally unique across providers — two
// registered providers may list the same ID — so every routing decision
// (clientForModel, model selection, session persistence) must carry the
// owning profile, not the bare ID.
//
// The wire body always carries WireID (the bare ID): endpoints never see
// the provider prefix. String is a display/selector form only and is never
// parsed back — model IDs may contain "/" (vLLM/llama.cpp ids), so a
// "provider/id" string is ambiguous to split; qualified selectors are
// resolved by prefix-matching registered profile names instead.
type ModelRef struct {
	// Profile is the registered provider profile name serving the model
	// (DefaultProfileName for the implicit legacy profile; "" is treated
	// as DefaultProfileName).
	Profile string
	// ID is the bare model ID as the endpoint lists/accepts it.
	ID string
}

// ProfileName returns the owning profile, normalizing "" to
// DefaultProfileName.
func (r ModelRef) ProfileName() string {
	if r.Profile == "" {
		return DefaultProfileName
	}
	return r.Profile
}

// Normalized returns the ref with its profile normalized ("" →
// DefaultProfileName). All routing maps (modelClient, modelProfile,
// effortsDerived) are keyed by the normalized form so a bare-ID ref and the
// same model's explicit default-profile ref hit the same entry.
func (r ModelRef) Normalized() ModelRef {
	r.Profile = r.ProfileName()
	return r
}

// WireID returns the bare model ID for the request body. The routing key
// and the wire ID must never be conflated: this is the only form an
// endpoint ever sees.
func (r ModelRef) WireID() string { return r.ID }

// Valid reports whether the ref names a model (an empty ID is the "no model
// selected" state).
func (r ModelRef) Valid() bool { return r.ID != "" }

// String is the display/selector form: "id" for the default profile,
// "profile/id" otherwise. Display only — never parsed (see the type
// comment).
func (r ModelRef) String() string {
	if p := r.ProfileName(); p != DefaultProfileName {
		return p + "/" + r.ID
	}
	return r.ID
}
