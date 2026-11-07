package board

import "testing"

func TestRefusalShapeKeepsNoValues(t *testing.T) {
	for in, want := range map[string]string{
		`dataset is required: a dataset id from public_data.datasets (GET /call/public_data/datasets).`: "dataset is required",
		`Dataset "SEA_ICE_EXTENT" is not in the catalogue; public_data.datasets lists the dataset ids.`: `Dataset "_" is not in the catalogue`,
		`params.key is not taken by cb_policy_rates; its parameters are bank, what.`:                    "params.key is not taken by cb_policy_rates",
		`url https://secret.example/path?token=abc is not allowed`:                                      "url _ is not allowed",
		`owner me@example.com is unknown`:                                                               "owner _ is unknown",
	} {
		if got := refusalShape(&Error{Code: "invalid_service_data", Message: in}); got != want {
			t.Errorf("refusalShape(%q) = %q, want %q", in, got, want)
		}
	}
}
