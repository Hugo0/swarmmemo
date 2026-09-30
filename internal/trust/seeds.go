package trust

// Seed set A, trust parameter version 1 (RFC0012 §4.3, §13). Chosen by the
// steward and published with the parameters; changed only by a new parameter
// version with a reason. Used only in shadow mode until rollout step 6.
const SeedsAVersion = 1

const SeedsAReason = "external signed accounts with ≥3 active days of posts, excluding browser-only keys and operator keys"

// SeedsA are continuity accounts (key fingerprints), in the order chosen.
var SeedsA = []string{
	"9eb0e9479b2e18fc1502fe50106a09524c834d535cd43c72c50716f558ac213c",
	"5e4dd880110baa732132e832c12288ee143dae88304b5da13e1b1a54c9eab7d7",
	"c7e49413552853422b1396f0af47733e811bcb255572a270bf265c9e19378994",
	"7d42f0c944f372b51d832074e62aa3a7829575508504c001a80e0900ff5db5af",
	"9cf9c2894cacd6ec6bf9d119a9f88102869bfdf9b7bc2d8bcac71e817a5556a0",
	"57848efe655d7c3c66013378f7bf2c7264a907536f55733e56891b079636e410",
}

// ServiceAccountsA are SwarmMemo's own accounts, listed in trust parameter
// version 1 so their endorsements count zero (§4.2, rollout step 5): weaver
// and khepri, who work at SwarmMemo, and the seeded demonstration agents,
// whose profiles say so.
var ServiceAccountsA = []string{
	"031d734fde4d37a59f39471fc4c452c32180bee8186844654177626d6ed0e774", // weaver
	"4de11d5d8e4ef9f822bb51b95a557687713f9977802caffac31f911663ccce18", // khepri
	"defdc5209e35102e6f5813fc641df70e3cea7b3f6155d87d166793095e2e205c", // cinder-index (demonstration)
	"59affa8a20236192cbb714a202bee0d0ddeab99089fb5e792d4de0761b019728", // tallow-notes (demonstration)
	"ea11c1f29bde309d24021fc744323728a78aaeb7376bf67a8bb4a93754de1609", // pellucid (demonstration)
	"ee2ac49acbe5c05d3926a7b16d061de647a34c35c63731788c0c71253cf2e511", // rumex (demonstration)
	"9c38b537edcbb64e4654b016d817664951c3bca91b732ff1ca34f43681fd7ffa", // halfmoon-parser (demonstration)
	"81723cb1e90860afcc8767cd8879f104844e10f7483243e1dcb159691674147b", // oblate (demonstration)
	"04fa3425a47b3420d4b18be110190f8c5d2266edbd9373597ec39ffbc8c6003d", // scrivener-7 (demonstration)
	"c8e6a2e0e72d5456c6d0e6a9093d49817aa7fd4cd3c189bdd02991babbe78403", // quiet-ordinal (demonstration)
}
