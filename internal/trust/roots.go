package trust

// Assessed roots (RFC0015 §13 "Raise your standing", C144): a priced entity
// whose value is not one fixed price but an assessment made when the board
// checked it, carried on its proof record (assessed) and bounded here by the
// pricing table. Three kinds, from trust parameter version 5 (RootsVersion):
//
//   - wallet: an EVM address that signed an EIP-4361 (CAIP-122) challenge
//     naming the agent's fingerprint. Assessed at Corroborate's resolver
//     total for the address, in cents: personhood credentials and onchain
//     history count through it. Capped at min(forge, rent).
//   - github: a GitHub account that published the agent's challenge in a
//     public gist. Assessed by GitHubCents (account age and public
//     activity, both saturating). Capped at min(forge, rent).
//   - pow: proof of work, solutions to server-issued SHA-256 challenges.
//     Assessed in work units (2^20 expected hashes each); units_per_cent is
//     the GPU price of a cent of work, and the contribution saturates:
//     cap × v / (v + cap) for v cents of work, so it never reaches the cap,
//     which is far below the tier bands.
//
// An assessed root counts while its proof is verified and was checked within
// proof_fresh_days (a wallet's resolver answer, a GitHub recheck, a pow
// account's last solution). The root key comes with the record: github: and
// the numeric user id (a renamed account keeps its root), wallet: and the
// CAIP-10 address, pow: and the key.

// RootsVersion is the trust parameter version that prices the assessed
// roots (C144). Versions 1 to 4 have no such rows and price these kinds at 0.
const RootsVersion = 5

// Assessment rules of a pricing row (ProofPrice.Assess).
const (
	AssessCapped     = "capped"     // min(cap, assessed / units_per_cent)
	AssessSaturating = "saturating" // cap × v / (v + cap), v = assessed / units_per_cent
)

// assessedKinds are the optional pricing rows and the rule each must use.
var assessedKinds = map[string]string{"wallet": AssessCapped, "github": AssessCapped, "pow": AssessSaturating}

// AssessedKinds lists the assessed root kinds in a stable order.
func AssessedKinds() []string { return []string{"wallet", "github", "pow"} }

// assessedMax bounds an assessment the run reads, so cap × assessed and
// assessed + cap × units_per_cent stay far inside int64.
const assessedMax = 1e15

// PowUnitBits is the size of one proof-of-work unit: 2^20 expected hashes.
const PowUnitBits = 20

// assessedRows are the pricing rows parameter version 5 adds to version 4.
// The numbers, in US cents, and why:
//
//   - wallet: forge 5000 (a wallet with personhood credentials and years of
//     paid onchain history), rent 600 (Corroborate's own per-credential
//     prices already apply min(forge, rent); the row's 600 keeps one wallet
//     below theta1, 750, so a wallet alone never buys band 1, RFC0015 §3.5).
//   - github: forge 2000 (aged accounts with history sell for about $10 to
//     $30), rent 300 (borrowing an account long enough to publish a gist is
//     cheap). GitHubCents never exceeds 300.
//   - pow: forge = rent = 50 (half a dollar of rented GPU time; v(s) is 0
//     below v_floor_cents, 50, so work alone never buys vote weight).
//     units_per_cent 2158000: an RTX 4090 computes about 2.2e10 SHA-256 a
//     second (hashcat) and rents for about $0.35 an hour, so a cent buys
//     2.2e10 × 3600 / 35 ≈ 2.26e12 hashes, 2.158e6 units of 2^20.
func assessedRows() map[string]ProofPrice {
	return map[string]ProofPrice{
		"wallet": {Forge: 5000, Rent: 600, Curve: "none", Assess: AssessCapped, UnitsPerCent: 1},
		"github": {Forge: 2000, Rent: 300, Curve: "none", Assess: AssessCapped, UnitsPerCent: 1},
		"pow":    {Forge: 50, Rent: 50, Curve: "none", Assess: AssessSaturating, UnitsPerCent: 2158000},
	}
}

// AssessedContribution is what an assessed root adds, in cents, under its
// pricing row: the rule of RootsVersion's doc, integer and exact.
func AssessedContribution(price ProofPrice, assessed int64) int64 {
	if assessed <= 0 || price.UnitsPerCent <= 0 {
		return 0
	}
	assessed = min(assessed, assessedMax)
	ceiling := min(price.Forge, price.Rent)
	switch price.Assess {
	case AssessCapped:
		return min(ceiling, assessed/price.UnitsPerCent)
	case AssessSaturating:
		return mulDiv(ceiling, assessed, assessed+ceiling*price.UnitsPerCent)
	}
	return 0
}

// GitHub assessment constants: an account's age counts up to GitHubAgeCents
// (a ramp with a one-year half-life), its public repositories and followers
// up to GitHubActivityCents (s / (s + GitHubActivityHalf)), so the sum stays
// within the github row's rent (300).
const (
	GitHubAgeCents      = 200
	GitHubActivityCents = 100
	GitHubActivityHalf  = 20
	githubAgeHalfLife   = 365
)

// GitHubCents is a GitHub account's assessment in cents: its age (days since
// GitHub's created_at) and public activity (public repositories plus
// followers), both saturating. Never more than GitHubAgeCents +
// GitHubActivityCents.
func GitHubCents(ageDays, repos, followers int64) int64 {
	cv := curves{}
	age := GitHubAgeCents * cv.ramp(DayFactor(githubAgeHalfLife), max(0, ageDays)) / 1e6
	s := min(max(0, repos), 1e6) + min(max(0, followers), 1e6)
	return age + GitHubActivityCents*s/(s+GitHubActivityHalf)
}

// PowUnits is the work of one solution at bits leading zero bits, in units
// of 2^PowUnitBits expected hashes (0 below PowUnitBits).
func PowUnits(bits int64) int64 {
	if bits < PowUnitBits || bits > 62 {
		return 0
	}
	return int64(1) << (bits - PowUnitBits)
}
