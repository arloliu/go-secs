package corpus

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// ErrorNotPrimary is the error of a lookup whose key names no primary (the tracepack corpus specification §5.11).
const ErrorNotPrimary = "not-primary"

// Role labels of a kept version, in the order of the role table of the tracepack corpus specification §5.11.
const (
	RolePrimary         = "primary"
	RoleCandidate       = "candidate"
	RolePossibleReply   = "possible-reply"
	RoleSameKeyPrimary  = "same-key-primary"
	RolePossiblePrimary = "possible-primary"
	RoleClosing         = "closing"
	RoleOutcome         = "outcome"
)

// roleRow is one row of the role table: a label and the TxClass bit of its role.
type roleRow struct {
	label string
	bit   tracepack.TxClass
}

// roleTable is the role table of the tracepack corpus specification §5.11, in its order.
var roleTable = []roleRow{
	{RolePrimary, tracepack.TxPrimary},
	{RoleCandidate, tracepack.TxCandidate},
	{RolePossibleReply, tracepack.TxPossibleReply},
	{RoleSameKeyPrimary, tracepack.TxSameKeyPrimary},
	{RolePossiblePrimary, tracepack.TxPossiblePrimary},
	{RoleClosing, tracepack.TxClosing},
	{RoleOutcome, tracepack.TxOutcomeRecord},
}

// outcomeNames maps each outcome of a lookup to its name in the tracepack corpus specification §5.11.
var outcomeNames = map[tracepack.TxOutcome]string{
	tracepack.TxMatched: "matched", tracepack.TxAmbiguous: "ambiguous",
	tracepack.TxUnmatched: "unmatched", tracepack.TxIncomplete: "incomplete",
}

// LookupVector is one transaction lookup of lookups.json (the tracepack corpus specification §5.11).
type LookupVector struct {
	ID        string       `json:"id"`
	Cites     []string     `json:"cites"`
	Source    LookupSource `json:"source"`
	Key       LookupKey    `json:"key"`
	MaxScopes int          `json:"max_scopes"`
	Expect    LookupExpect `json:"expect"`
}

// Lookups is lookups.json: the transaction lookups over the packs of a multi-pack vector.
type Lookups []LookupVector

// LookupSource is the source of a lookup's observation, by pack number (the tracepack corpus specification §5.11 and §8).
type LookupSource struct {
	View     []int `json:"view"`
	Evidence []int `json:"evidence"`
	Complete bool  `json:"complete"`
}

// LookupKey names a lookup's primary: its capture, its seq and the hour of its scope.
type LookupKey struct {
	CaptureID string `json:"capture_id"`
	Seq       U64    `json:"seq"`
	Hour      I64    `json:"hour"`
}

// LookupExpect is what a lookup yields: the error form, {error},
// or the result, {outcome, key?, window_end?, records, gaps, searched, conflicts, footer_errors}.
// The result holds Outcome and its five arrays, each non-nil, and no Error; the error form holds Error alone.
type LookupExpect struct {
	// Error is the error form's error, ErrorNotPrimary; "" in the result.
	Error        string              `json:"error,omitempty"`
	Outcome      string              `json:"outcome,omitempty"`
	Key          *PrimaryKey         `json:"key,omitempty"`
	WindowEnd    *U64                `json:"window_end,omitempty"`
	Records      *[]LookupRecord     `json:"records,omitempty"`
	Gaps         *[]Fact             `json:"gaps,omitempty"`
	Searched     *[]SearchedScope    `json:"searched,omitempty"`
	Conflicts    *[]ConflictEntry    `json:"conflicts,omitempty"`
	FooterErrors *[]ScopeFooterError `json:"footer_errors,omitempty"`
}

// PrimaryKey is the primary's fields, from the first kept version of the primary,
// each HSMS header field present iff available on that version.
type PrimaryKey struct {
	Epoch       uint32  `json:"epoch"`
	Dir         string  `json:"dir"`
	SessionID   *uint16 `json:"session_id,omitempty"`
	SystemBytes []byte  `json:"system_bytes,omitempty"`
	Stream      *uint8  `json:"stream,omitempty"`
	Function    *uint8  `json:"function,omitempty"`
	W           *bool   `json:"w,omitempty"`
}

// LookupRecord is one version a lookup kept.
type LookupRecord struct {
	Seq      U64      `json:"seq"`
	Hour     I64      `json:"hour"`
	Pack     int      `json:"pack"`
	Block    int      `json:"block"`
	Conflict bool     `json:"conflict"`
	Roles    []string `json:"roles"`
	InWindow bool     `json:"in_window"`
	Bound    bool     `json:"bound"`
	// Decidable, Eligible and Valid are present iff Roles holds RoleCandidate.
	Decidable *bool `json:"decidable,omitempty"`
	Eligible  *bool `json:"eligible,omitempty"`
	Valid     *bool `json:"valid,omitempty"`
}

// SearchedScope is one scope a lookup read to its end:
// its hour, whether it is indexed, and its view's packs in view order.
type SearchedScope struct {
	Hour    I64   `json:"hour"`
	Indexed bool  `json:"indexed"`
	Packs   []int `json:"packs"`
}

// ScopeFooterError names a pack of a scope read whose footer the read did not use.
type ScopeFooterError struct {
	Hour I64 `json:"hour"`
	Pack int `json:"pack"`
}

// LookupRun is a lookup as tracepack.FindTransaction returned it, with what its projection needs.
type LookupRun struct {
	// Key is the key FindTransaction was given.
	Key tracepack.TxKey
	// MaxScopes is the TxOptions.MaxScopes FindTransaction was given, at least 1.
	MaxScopes int
	// Numbers maps the pack_id of every source pack to its number.
	Numbers map[tracepack.UUID]int
	// Result and Err are what FindTransaction returned.
	Result tracepack.TxResult
	Err    error
}

// Lookup returns the key and options of FindTransaction that l states.
//
// Returns:
//   - tracepack.TxKey: the key.
//   - tracepack.TxOptions: MaxScopes; no budget and no retention.
//   - error: a capture_id that is no UUID in canonical form, max_scopes below 1,
//     or hours scheduled, key.hour to key.hour + max_scopes - 1, outside [tracepack.MinTxHour, tracepack.MaxTxHour].
func (l *LookupVector) Lookup() (tracepack.TxKey, tracepack.TxOptions, error) {
	capture, err := parseUUID(l.Key.CaptureID)
	if err != nil {
		return tracepack.TxKey{}, tracepack.TxOptions{}, fmt.Errorf("corpus: lookup %q: %w", l.ID, err)
	}
	if l.MaxScopes < 1 {
		return tracepack.TxKey{}, tracepack.TxOptions{}, fmt.Errorf("corpus: lookup %q: max_scopes %d is below 1", l.ID, l.MaxScopes)
	}
	if h := int64(l.Key.Hour); h < tracepack.MinTxHour || h > tracepack.MaxTxHour || int64(l.MaxScopes) > tracepack.MaxTxHour+1-h {
		return tracepack.TxKey{}, tracepack.TxOptions{}, fmt.Errorf("corpus: lookup %q: hour %d and max_scopes %d schedule hours out of the range of a lookup",
			l.ID, l.Key.Hour, l.MaxScopes)
	}

	return tracepack.TxKey{Capture: capture, Seq: uint64(l.Key.Seq), Hour: int64(l.Key.Hour)}, tracepack.TxOptions{MaxScopes: l.MaxScopes}, nil
}

// LookupExpectFrom returns the expect of the lookup run (the tracepack corpus specification §5.11):
// the error form for an error wrapping tracepack.ErrNotPrimary;
// otherwise the outcome, the primary's fields from the first kept version of the primary,
// the kept versions with their roles and flags, the canonical facts (NormalizeGaps),
// the scopes searched, the conflicts and the footer errors, every pack by its number.
//
// Returns:
//   - LookupExpect: the projection.
//   - error: run's error other than ErrNotPrimary;
//     or an inconsistency of the result: a pack_id no source pack has, an undefined outcome or role bit,
//     candidate flags on a version that is not a candidate, a first kept version that is not the primary's,
//     key fields that disagree with that version, versions, scopes or footer errors out of their order,
//     a conflict naming packs of no scope searched, a footer error naming no pack of a scope searched,
//     an outcome its facts and valid matches contradict, or the error of NormalizeGaps.
func LookupExpectFrom(run *LookupRun) (LookupExpect, error) {
	if run.Err != nil {
		if errors.Is(run.Err, tracepack.ErrNotPrimary) {
			return LookupExpect{Error: ErrorNotPrimary}, nil
		}

		return LookupExpect{}, fmt.Errorf("corpus: the lookup failed: %w", run.Err)
	}
	res := &run.Result
	outcome, ok := outcomeNames[res.Outcome]
	if !ok {
		return LookupExpect{}, fmt.Errorf("corpus: the lookup's outcome %v is not defined", res.Outcome)
	}
	number := func(id tracepack.UUID) (int, error) {
		n, ok := run.Numbers[id]
		if !ok {
			return 0, fmt.Errorf("corpus: pack %s is no source pack", id)
		}

		return n, nil
	}

	out := LookupExpect{Outcome: outcome}
	key, err := primaryKey(run)
	if err != nil {
		return LookupExpect{}, err
	}
	out.Key = key
	if res.WindowEnd != nil {
		out.WindowEnd = new(U64(*res.WindowEnd))
	}

	records, err := lookupRecords(res.Records, number)
	if err != nil {
		return LookupExpect{}, err
	}
	gaps, err := NormalizeGaps(run)
	if err != nil {
		return LookupExpect{}, err
	}
	searched, err := searchedScopes(res.Searched, number)
	if err != nil {
		return LookupExpect{}, err
	}
	conflicts, err := scopeConflicts(res.Conflicts, res.Searched, number)
	if err != nil {
		return LookupExpect{}, err
	}
	footer, err := scopeFooterErrors(res.FooterErrs, searched, number)
	if err != nil {
		return LookupExpect{}, err
	}
	if err := checkOutcome(outcome, gaps, validMatches(records)); err != nil {
		return LookupExpect{}, err
	}
	out.Records, out.Gaps, out.Searched, out.Conflicts, out.FooterErrors = &records, &gaps, &searched, &conflicts, &footer

	return out, nil
}

// primaryKey returns the primary's fields of run's result, nil when the lookup kept no version of the primary:
// each field present iff available on the first kept version, which must be the primary's,
// and the result's fields must be those of that version, an unavailable one zero.
func primaryKey(run *LookupRun) (*PrimaryKey, error) {
	res := &run.Result
	if len(res.Records) == 0 {
		return nil, nil //nolint:nilnil // no version of the primary kept is no key, not an error
	}
	first := &res.Records[0].Record
	if first.Seq != run.Key.Seq {
		return nil, fmt.Errorf("corpus: the first kept version is seq %d, not the primary's %d", first.Seq, run.Key.Seq)
	}
	h := first.HSMSHeader()
	has := func(bit tracepack.FieldValidity) bool { return h.Available&bit != 0 }

	// keyFields holds the fields of a TxResult that the primary's first version decides.
	type keyFields struct {
		Epoch                       uint32
		Dir                         tracepack.Dir
		SessionID                   uint16
		SystemBytes                 [4]byte
		Stream, Function            uint8
		W                           bool
		StreamAvailable, WAvailable bool
	}
	want := keyFields{Epoch: first.Epoch, Dir: first.Dir}
	k := &PrimaryKey{Epoch: first.Epoch, Dir: first.Dir.String()}
	if has(tracepack.FieldValiditySessionID) {
		want.SessionID, k.SessionID = h.SessionID, new(h.SessionID)
	}
	if has(tracepack.FieldValiditySystemBytes) {
		want.SystemBytes, k.SystemBytes = h.SystemBytes, slices.Clone(h.SystemBytes[:])
	}
	if has(tracepack.FieldValidityFunction) {
		want.Function, k.Function = h.Function, new(h.Function)
	}
	if has(tracepack.FieldValidityStreamAndW) {
		want.Stream, want.W, want.StreamAvailable, want.WAvailable = h.Stream, h.W, true, true
		k.Stream, k.W = new(h.Stream), new(h.W)
	}
	got := keyFields{
		Epoch: res.Epoch, Dir: res.Dir, SessionID: res.SessionID, SystemBytes: res.SystemBytes, Function: res.Function,
		Stream: res.Stream, W: res.W, StreamAvailable: res.StreamAvailable, WAvailable: res.WAvailable,
	}
	if want != got {
		return nil, fmt.Errorf("corpus: the result's key fields differ from the primary's first version")
	}

	return k, nil
}

// lookupRecords renders the kept versions rs, which must ascend by seq, then hour.
func lookupRecords(rs []tracepack.TxRecord, number func(tracepack.UUID) (int, error)) ([]LookupRecord, error) {
	out := make([]LookupRecord, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		if i > 0 {
			prev := &rs[i-1]
			if cmp.Or(cmp.Compare(r.Record.Seq, prev.Record.Seq), cmp.Compare(r.Hour, prev.Hour)) < 0 {
				return nil, fmt.Errorf("corpus: kept version %d (seq %d, hour %d) is out of order", i, r.Record.Seq, r.Hour)
			}
		}
		pack, err := number(r.Pack)
		if err != nil {
			return nil, err
		}
		roles, err := rolesOf(r.Class)
		if err != nil {
			return nil, err
		}
		x := LookupRecord{
			Seq: U64(r.Record.Seq), Hour: I64(r.Hour), Pack: pack, Block: r.Block, Conflict: r.Conflict,
			Roles: roles, InWindow: r.InWindow, Bound: r.Bound,
		}
		if r.Class.Has(tracepack.TxCandidate) {
			x.Decidable, x.Eligible, x.Valid = new(r.Decidable), new(r.Eligible), new(r.Valid)
		} else if r.Decidable || r.Eligible || r.Valid {
			return nil, fmt.Errorf("corpus: kept version at seq %d has candidate flags but is no candidate", r.Record.Seq)
		}
		out = append(out, x)
	}

	return out, nil
}

// rolesOf returns the role labels of class, in the order of the role table.
func rolesOf(class tracepack.TxClass) ([]string, error) {
	roles := []string{}
	var known tracepack.TxClass
	for _, r := range roleTable {
		known |= r.bit
		if class.Has(r.bit) {
			roles = append(roles, r.label)
		}
	}
	if class&^known != 0 {
		return nil, fmt.Errorf("corpus: role bits %#x are not defined", uint16(class&^known))
	}

	return roles, nil
}

// searchedScopes renders the scopes searched ss, which must ascend by hour.
func searchedScopes(ss []tracepack.TxScope, number func(tracepack.UUID) (int, error)) ([]SearchedScope, error) {
	out := make([]SearchedScope, 0, len(ss))
	for i := range ss {
		s := &ss[i]
		if i > 0 && s.Hour <= ss[i-1].Hour {
			return nil, fmt.Errorf("corpus: the scope of hour %d is searched out of order", s.Hour)
		}
		packs := make([]int, 0, len(s.Packs))
		for _, id := range s.Packs {
			n, err := number(id)
			if err != nil {
				return nil, err
			}
			packs = append(packs, n)
		}
		out = append(out, SearchedScope{Hour: I64(s.Hour), Indexed: s.Indexed, Packs: packs})
	}

	return out, nil
}

// scopeConflicts renders the conflicts cs, each in the view order of the scope searched that holds its first pack;
// the scopes of successive conflicts must not go back.
func scopeConflicts(cs []tracepack.Conflict, ss []tracepack.TxScope, number func(tracepack.UUID) (int, error)) ([]ConflictEntry, error) {
	out := make([]ConflictEntry, 0, len(cs))
	last := 0
	for i := range cs {
		c := &cs[i]
		if len(c.Versions) == 0 || len(c.Versions[0]) == 0 {
			return nil, fmt.Errorf("corpus: the conflict at seq %d lists no version", c.Seq)
		}
		k := slices.IndexFunc(ss, func(s tracepack.TxScope) bool { return slices.Contains(s.Packs, c.Versions[0][0]) })
		if k < last {
			return nil, fmt.Errorf("corpus: the conflict at seq %d names a pack of no scope searched, or out of read order", c.Seq)
		}
		last = k
		numbers := make([]int, 0, len(ss[k].Packs))
		for _, id := range ss[k].Packs {
			n, err := number(id)
			if err != nil {
				return nil, err
			}
			numbers = append(numbers, n)
		}
		e, err := conflictEntries(cs[i:i+1], ss[k].Packs, numbers)
		if err != nil {
			return nil, err
		}
		out = append(out, e...)
	}

	return out, nil
}

// scopeFooterErrors renders the footer errors fs,
// each of a pack of a scope searched, which must ascend by hour, then by view order.
func scopeFooterErrors(fs []tracepack.TxPackError, searched []SearchedScope, number func(tracepack.UUID) (int, error)) ([]ScopeFooterError, error) {
	out := make([]ScopeFooterError, 0, len(fs))
	for i := range fs {
		n, err := number(fs[i].Pack)
		if err != nil {
			return nil, err
		}
		e := ScopeFooterError{Hour: I64(fs[i].Hour), Pack: n}
		if !ofScopeSearched(&e, searched) {
			return nil, fmt.Errorf("corpus: the footer error of pack %d in hour %d names no pack of a scope searched", n, fs[i].Hour)
		}
		if i > 0 && compareFooterErrors(&out[i-1], &e, searched) >= 0 {
			return nil, fmt.Errorf("corpus: the footer error of pack %d in hour %d is out of order", n, fs[i].Hour)
		}
		out = append(out, e)
	}

	return out, nil
}

// ofScopeSearched reports whether the footer error e names a pack of the scope of its hour in searched.
func ofScopeSearched(e *ScopeFooterError, searched []SearchedScope) bool {
	k := slices.IndexFunc(searched, func(s SearchedScope) bool { return s.Hour == e.Hour })

	return k >= 0 && slices.Contains(searched[k].Packs, e.Pack)
}

// compareFooterErrors orders footer errors by hour, then by the view order of their scope in searched;
// two of one hour whose scope was not searched compare equal.
func compareFooterErrors(a, b *ScopeFooterError, searched []SearchedScope) int {
	if c := cmp.Compare(a.Hour, b.Hour); c != 0 {
		return c
	}
	k := slices.IndexFunc(searched, func(s SearchedScope) bool { return s.Hour == a.Hour })
	if k < 0 {
		return 0
	}

	return cmp.Compare(slices.Index(searched[k].Packs, a.Pack), slices.Index(searched[k].Packs, b.Pack))
}

// checkOutcome checks that outcome follows from the facts gaps and the number of valid matches valid
// (the tracepack corpus specification §5.11, Facts):
// incomplete with a no-key, conflict, index or scope-breach fact;
// otherwise matched with one valid match and ambiguous with several;
// otherwise unmatched without facts and incomplete with them.
func checkOutcome(outcome string, gaps []Fact, valid int) error {
	want := "incomplete"
	switch {
	case slices.ContainsFunc(gaps, func(f Fact) bool { return slices.Contains(incompleteFacts, f.Reason) }):
		// A fact that makes the outcome incomplete decides it.
	case valid == 1:
		want = "matched"
	case valid > 1:
		want = "ambiguous"
	case len(gaps) == 0:
		want = "unmatched"
	default:
		// Facts without a valid match leave the outcome incomplete.
	}
	if outcome != want {
		return fmt.Errorf("corpus: outcome %s beside %d valid matches and %d facts, which give %s", outcome, valid, len(gaps), want)
	}

	return nil
}

// validMatches returns the number of kept versions whose valid flag is true.
func validMatches(records []LookupRecord) int {
	n := 0
	for _, r := range records {
		if r.Valid != nil && *r.Valid {
			n++
		}
	}

	return n
}

// Marshal returns lookups.json in the canonical form:
// the lookups ascending by id, which must be unique kebab-case ids,
// each result's roles in the order of the role table and its gaps checked, sorted and each distinct fact once.
// ls itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: a lookup that check refuses.
func (ls Lookups) Marshal() ([]byte, error) {
	out := slices.Clone(ls)
	slices.SortFunc(out, func(a, b LookupVector) int { return strings.Compare(a.ID, b.ID) })
	for i := range out {
		if i > 0 && out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("corpus: lookup %q is listed twice", out[i].ID)
		}
		e, err := out[i].normalized()
		if err != nil {
			return nil, err
		}
		out[i].Expect = e
	}
	if out == nil {
		out = Lookups{}
	}

	return Marshal([]LookupVector(out))
}

// normalized checks l against the schema of the tracepack corpus specification §5.11
// and returns its expect with its roles and gaps in their canonical orders.
func (l *LookupVector) normalized() (LookupExpect, error) {
	if !kebabID.MatchString(l.ID) || len(l.Cites) == 0 {
		return LookupExpect{}, fmt.Errorf("corpus: lookup %q needs a kebab-case id and at least one cite", l.ID)
	}
	src := &l.Source
	all := append(slices.Clone(src.View), src.Evidence...)
	switch {
	case len(src.View) == 0 || src.Evidence == nil:
		return LookupExpect{}, fmt.Errorf("corpus: lookup %q: view is empty or evidence absent", l.ID)
	case slices.ContainsFunc(all, func(n int) bool { return n < 0 }) || hasDuplicate(all):
		return LookupExpect{}, fmt.Errorf("corpus: lookup %q: source names a negative pack or a pack twice", l.ID)
	}
	if _, _, err := l.Lookup(); err != nil {
		return LookupExpect{}, err
	}

	e := l.Expect
	if e.Error != "" {
		if e.Error != ErrorNotPrimary || e.Outcome != "" || e.Key != nil || e.WindowEnd != nil || e.Records != nil ||
			e.Gaps != nil || e.Searched != nil || e.Conflicts != nil || e.FooterErrors != nil {
			return LookupExpect{}, fmt.Errorf("corpus: lookup %q: the error form holds only error %q", l.ID, ErrorNotPrimary)
		}

		return e, nil
	}
	if err := l.normalResult(&e); err != nil {
		return LookupExpect{}, fmt.Errorf("corpus: lookup %q: %w", l.ID, err)
	}

	return e, nil
}

// normalResult checks the result e and puts its roles and gaps in their canonical orders.
func (l *LookupVector) normalResult(e *LookupExpect) error {
	if !slices.Contains(slices.Collect(maps.Values(outcomeNames)), e.Outcome) {
		return fmt.Errorf("outcome %q is not defined", e.Outcome)
	}
	if e.Records == nil || e.Gaps == nil || e.Searched == nil || e.Conflicts == nil || e.FooterErrors == nil {
		return errors.New("the result needs records, gaps, searched, conflicts and footer_errors")
	}
	if err := l.checkKey(e.Key); err != nil {
		return err
	}
	records, err := l.normalRecords(*e.Records)
	if err != nil {
		return err
	}
	gaps, err := canonicalFacts(*e.Gaps)
	if err != nil {
		return err
	}
	for _, f := range gaps {
		if f.Pack != nil && !slices.Contains(l.Source.View, *f.Pack) {
			return fmt.Errorf("a %s fact names pack %d, not in view", f.Reason, *f.Pack)
		}
	}
	if err := checkOutcome(e.Outcome, gaps, validMatches(records)); err != nil {
		return err
	}
	if err := l.checkWindow(records, gaps, e.WindowEnd, *e.Searched); err != nil {
		return err
	}
	if err := l.checkScopes(*e.Searched, *e.Conflicts, *e.FooterErrors); err != nil {
		return err
	}
	if err := l.checkSchedule(gaps, *e.Searched); err != nil {
		return err
	}
	if err := checkReadPlaces(records, gaps, *e.Searched); err != nil {
		return err
	}
	e.Records, e.Gaps = &records, &gaps

	return nil
}

// perReadFacts are the reasons of the facts that one scope read records, each naming that read's hour alone
// (the tracepack corpus specification §5.11, Facts).
var perReadFacts = []string{FactIndex, FactScopeBreach, FactRead, FactCoverage, FactUnevaluated, FactUnavailable}

// checkReadPlaces checks that each kept version and each fact of one scope read names a scope searched,
// and a pack of that scope's view where it names one:
// a version is kept from a scope read, and such a fact is recorded by one.
func checkReadPlaces(records []LookupRecord, gaps []Fact, searched []SearchedScope) error {
	inScope := func(hour I64, pack *int) bool {
		k := slices.IndexFunc(searched, func(s SearchedScope) bool { return s.Hour == hour })

		return k >= 0 && (pack == nil || slices.Contains(searched[k].Packs, *pack))
	}
	for i := range records {
		if r := &records[i]; !inScope(r.Hour, &r.Pack) {
			return fmt.Errorf("record at seq %d names hour %d and pack %d, no pack of a scope searched", r.Seq, r.Hour, r.Pack)
		}
	}
	for i := range gaps {
		f := &gaps[i]
		if slices.Contains(perReadFacts, f.Reason) && (len(f.Hours) != 1 || !inScope(f.Hours[0], f.Pack)) {
			return fmt.Errorf("a %s fact names hours %v and a pack of no scope searched", f.Reason, f.Hours)
		}
	}

	return nil
}

// checkKey checks the primary's fields k, which may be nil.
func (l *LookupVector) checkKey(k *PrimaryKey) error {
	if k == nil {
		return nil
	}
	if _, ok := enumByName[tracepack.Dir](k.Dir); !ok {
		return fmt.Errorf("key dir %q is not defined", k.Dir)
	}
	switch {
	case k.SystemBytes != nil && len(k.SystemBytes) != systemBytesLen:
		return fmt.Errorf("key system_bytes holds %d bytes", len(k.SystemBytes))
	case (k.Stream == nil) != (k.W == nil):
		return errors.New("key stream and w are present together or not at all")
	case k.Stream != nil && *k.Stream > 0x7F:
		return fmt.Errorf("key stream %d takes more than 7 bits", *k.Stream)
	}

	return nil
}

// normalRecords checks the kept versions rs and returns them with their roles in the order of the role table.
func (l *LookupVector) normalRecords(rs []LookupRecord) ([]LookupRecord, error) {
	out := make([]LookupRecord, 0, len(rs))
	for i, r := range rs {
		if i > 0 && cmp.Or(cmp.Compare(r.Seq, rs[i-1].Seq), cmp.Compare(r.Hour, rs[i-1].Hour)) < 0 {
			return nil, fmt.Errorf("record %d (seq %d, hour %d) is out of order", i, r.Seq, r.Hour)
		}
		if !slices.Contains(l.Source.View, r.Pack) || r.Block < 0 || r.Roles == nil {
			return nil, fmt.Errorf("record at seq %d names pack %d, block %d, or has no roles", r.Seq, r.Pack, r.Block)
		}
		if (r.Seq == l.Key.Seq) != slices.Contains(r.Roles, RolePrimary) {
			return nil, fmt.Errorf("record at seq %d: a version is labelled primary iff it is at the primary's seq %d", r.Seq, l.Key.Seq)
		}
		ranks := make([]int, 0, len(r.Roles))
		for _, role := range r.Roles {
			k := slices.IndexFunc(roleTable, func(x roleRow) bool { return x.label == role })
			if k < 0 {
				return nil, fmt.Errorf("role %q is not defined", role)
			}
			ranks = append(ranks, k)
		}
		if hasDuplicate(ranks) {
			return nil, fmt.Errorf("record at seq %d holds a role twice", r.Seq)
		}
		slices.Sort(ranks)
		r.Roles = []string{}
		for _, k := range ranks {
			r.Roles = append(r.Roles, roleTable[k].label)
		}
		candidate := slices.Contains(r.Roles, RoleCandidate)
		if flags := r.Decidable != nil && r.Eligible != nil && r.Valid != nil; flags != candidate ||
			!candidate && (r.Decidable != nil || r.Eligible != nil || r.Valid != nil) {
			return nil, fmt.Errorf("record at seq %d: the candidate flags are present iff it is a candidate", r.Seq)
		}
		out = append(out, r)
	}

	return out, nil
}

// checkScopes checks the scopes searched, the conflicts and the footer errors of a result against their orders,
// and that each names packs of the view.
func (l *LookupVector) checkScopes(searched []SearchedScope, conflicts []ConflictEntry, footer []ScopeFooterError) error {
	for i, s := range searched {
		if i > 0 && s.Hour <= searched[i-1].Hour {
			return fmt.Errorf("searched hour %d is out of order", s.Hour)
		}
		if s.Packs == nil || hasDuplicate(s.Packs) {
			return fmt.Errorf("searched hour %d has no packs or names one twice", s.Hour)
		}
		for _, n := range s.Packs {
			if !slices.Contains(l.Source.View, n) {
				return fmt.Errorf("searched hour %d names pack %d, not in view", s.Hour, n)
			}
		}
	}
	for _, c := range conflicts {
		if _, err := parseUUID(c.CaptureID); err != nil {
			return err
		}
		if len(c.Versions) == 0 {
			return fmt.Errorf("the conflict at seq %d lists no version", c.Seq)
		}
		for _, v := range c.Versions {
			if len(v) == 0 || hasDuplicate(v) || slices.ContainsFunc(v, func(n int) bool { return !slices.Contains(l.Source.View, n) }) {
				return fmt.Errorf("a version of the conflict at seq %d names no pack, a pack twice, or a pack not in view", c.Seq)
			}
		}
	}
	for i := range footer {
		if !ofScopeSearched(&footer[i], searched) || i > 0 && compareFooterErrors(&footer[i-1], &footer[i], searched) >= 0 {
			return fmt.Errorf("the footer error of pack %d in hour %d names no pack of a scope searched, or is out of order", footer[i].Pack, footer[i].Hour)
		}
	}

	return nil
}

// checkSchedule checks the hours of a result's scopes searched and conflicted facts against the hours scheduled,
// key.hour and the max_scopes - 1 after it (the tracepack semantics specification §7.2, Scopes read):
// each searched or conflicted hour is scheduled, and no hour is both, since a conflicted scope is not read;
// unless the result is an early return, every hour scheduled is searched or conflicted, as such a lookup reads every hour scheduled
// but a conflicted one.
// The scopes searched are distinct and the facts canonical, so the hours are counted, not looped over.
func (l *LookupVector) checkSchedule(gaps []Fact, searched []SearchedScope) error {
	first, last := l.Key.Hour, l.Key.Hour+I64(l.MaxScopes)-1
	for _, s := range searched {
		if s.Hour < first || s.Hour > last {
			return fmt.Errorf("searched hour %d is not scheduled", s.Hour)
		}
	}
	conflicted := 0
	for _, f := range gaps {
		if f.Reason != FactConflicted {
			continue
		}
		h := f.Hours[0]
		switch {
		case h < first || h > last:
			return fmt.Errorf("conflicted hour %d is not scheduled", h)
		case slices.ContainsFunc(searched, func(s SearchedScope) bool { return s.Hour == h }):
			return fmt.Errorf("conflicted hour %d is also searched, though a conflicted scope is not read", h)
		default:
			conflicted++
		}
	}
	if !earlyReturn(gaps, l.Key.Seq) && len(searched)+conflicted != l.MaxScopes {
		return fmt.Errorf("%d hours searched and %d conflicted, not every one of the %d hours scheduled", len(searched), conflicted, l.MaxScopes)
	}

	return nil
}

// notReportedEarly are the reasons of the facts an early-return result never holds,
// besides a conflict above the primary's seq (the tracepack corpus specification §5.11, Early-return results).
var notReportedEarly = []string{
	FactCoverage, FactBarrier, FactCaptureBoundary, FactOrderingUncertain, FactEvidence,
	FactSeqGap, FactOpenWindow, FactUnavailable, FactContradiction,
}

// earlyReturn reports whether a result whose facts are gaps ended after the scope of the primary's hour
// (the tracepack corpus specification §5.11, Early-return results):
// its primary has no key, and not because a later scope's read yields another version of it,
// which a conflict at the primary's seq over several hours states.
func earlyReturn(gaps []Fact, p U64) bool {
	noKey := slices.ContainsFunc(gaps, func(f Fact) bool { return f.Reason == FactNoKey })
	across := slices.ContainsFunc(gaps, func(f Fact) bool {
		return f.Reason == FactConflict && f.Seq != nil && *f.Seq == p && len(f.Hours) > 1
	})

	return noKey && !across
}

// boundsWindow reports whether the kept version r can bound the window:
// a same-key primary, with or without a conflict, or a version labelled closing without a conflict
// (the tracepack corpus specification §5.11, Window).
func boundsWindow(r *LookupRecord) bool {
	return slices.Contains(r.Roles, RoleSameKeyPrimary) || slices.Contains(r.Roles, RoleClosing) && !r.Conflict
}

// checkWindow checks the window of a result against its kept versions records, ascending by seq, and its facts gaps
// (the tracepack corpus specification §5.11, Window and Early-return results).
// An early return has no window end, keeps only versions of the primary, none in the window or bounding it,
// searches no scope but the primary's, and holds none of the facts it does not report.
// Any other result's window end is the smallest seq above the primary's of a version that can bound the window,
// absent when there is none;
// a version is in the window when its seq lies above the primary's and below the end, if any,
// and bounds it when it can and its seq is the end.
func (l *LookupVector) checkWindow(records []LookupRecord, gaps []Fact, windowEnd *U64, searched []SearchedScope) error {
	p := l.Key.Seq
	if earlyReturn(gaps, p) {
		return l.checkEarlyReturn(records, gaps, windowEnd, searched)
	}

	var end *U64
	for i := range records {
		if records[i].Seq > p && boundsWindow(&records[i]) {
			end = &records[i].Seq
			break
		}
	}
	if (end == nil) != (windowEnd == nil) || end != nil && *end != *windowEnd {
		return fmt.Errorf("the window end is not the smallest seq above %d of a version that bounds the window", p)
	}
	for i := range records {
		r := &records[i]
		in := r.Seq > p && (end == nil || r.Seq < *end)
		bound := end != nil && r.Seq == *end && boundsWindow(r)
		if r.InWindow != in || r.Bound != bound {
			return fmt.Errorf("record at seq %d: in_window %v and bound %v, the window gives %v and %v", r.Seq, r.InWindow, r.Bound, in, bound)
		}
	}

	return nil
}

// checkEarlyReturn checks the shape of an early-return result (the tracepack corpus specification §5.11, Early-return results):
// no window end, only versions of the primary, none in the window or bounding it,
// no scope searched but the primary's, and none of the facts an early return does not report.
func (l *LookupVector) checkEarlyReturn(records []LookupRecord, gaps []Fact, windowEnd *U64, searched []SearchedScope) error {
	p := l.Key.Seq
	switch {
	case windowEnd != nil:
		return errors.New("an early return has no window end")
	case slices.ContainsFunc(records, func(r LookupRecord) bool { return r.Seq != p || r.InWindow || r.Bound }):
		return errors.New("an early return keeps only versions of the primary, none in the window or bounding it")
	case len(searched) > 1 || len(searched) == 1 && searched[0].Hour != l.Key.Hour:
		return errors.New("an early return searches no scope but the primary's")
	case slices.ContainsFunc(gaps, func(f Fact) bool {
		return slices.Contains(notReportedEarly, f.Reason) || f.Reason == FactConflict && f.Seq != nil && *f.Seq > p
	}):
		return errors.New("an early return holds a fact it does not report")
	default:
		return nil
	}
}
