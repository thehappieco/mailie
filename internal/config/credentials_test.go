package config_test

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/config"
)

// credentialKeyARN is a key ARN in AWS's documentation account.
const credentialKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func TestWithAKMSKeyTheCredentialKeyIsOptional(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV":                    "prod",
		"MAIL_CREDENTIAL_KMS_KEY_ARN": credentialKeyARN,
		"MAIL_DATA_DIR":               t.TempDir(),
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Credentials
	if c.KMSKeyARN != credentialKeyARN || c.KMSRegion != "eu-west-1" || len(c.Keys) != 0 || c.Sealer() != config.SealerAWSKMS {
		t.Fatalf("credentials: %+v, sealer %s", c, c.Sealer())
	}

	// The log line names what seals, never the key's account.
	out := cfg.String()
	for _, want := range []string{"credential_sealer=aws-kms", "credential_keys=none"} {
		if !strings.Contains(out, want) {
			t.Errorf("String() does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "111122223333") {
		t.Errorf("String() names the key's account:\n%s", out)
	}
}

func TestUnderAKMSKeyTheCredentialKeysGivenOnlyOpen(t *testing.T) {
	older := strings.Repeat("22", 32)
	for name, vars := range map[string]map[string]string{
		"the active key and a previous one": {
			"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_CREDENTIAL_KEY_ID": "2", "MAIL_CREDENTIAL_PREVIOUS_KEYS": "1:" + older,
		},
		// Without MAIL_CREDENTIAL_KEY_HEX no id is taken: key 1, the
		// default active id, may be a previous key.
		"previous keys alone": {"MAIL_CREDENTIAL_PREVIOUS_KEYS": "1:" + older + ",2:" + validKey},
	} {
		t.Run(name, func(t *testing.T) {
			vars["MAIL_ENV"] = "dev"
			vars["MAIL_CREDENTIAL_KMS_KEY_ARN"] = credentialKeyARN
			vars["MAIL_DATA_DIR"] = t.TempDir()
			setenv(t, vars)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Credentials.Keys) != 2 || cfg.Credentials.Sealer() != config.SealerAWSKMS {
				t.Fatalf("keys %d, sealer %s; want both keys, beside the KMS key", len(cfg.Credentials.Keys), cfg.Credentials.Sealer())
			}
			// No keyring key is marked as the one that seals.
			if out := cfg.String(); !strings.Contains(out, "credential_sealer=aws-kms credential_keys=1,2 ") {
				t.Errorf("String():\n%s", out)
			}
		})
	}
}

func TestWithoutAKMSKeyTheKeyringSealsAsBefore(t *testing.T) {
	setenv(t, map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_DATA_DIR": t.TempDir()})
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Credentials.KMSKeyARN != "" || cfg.Credentials.Sealer() != config.SealerKeyring {
		t.Fatalf("credentials: %+v", cfg.Credentials)
	}
	if out := cfg.String(); !strings.Contains(out, "credential_sealer=keyring credential_keys=1* ") {
		t.Errorf("String():\n%s", out)
	}
}

func TestTheCredentialKMSKeyMustBeAKeysARN(t *testing.T) {
	for name, arn := range map[string]string{
		"an alias":              "arn:aws:kms:eu-west-1:111122223333:alias/mail-credentials",
		"a bare key id":         "1234abcd-12ab-34cd-56ef-1234567890ab",
		"a short account":       "arn:aws:kms:eu-west-1:1111:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"another service":       "arn:aws:s3:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"no region":             "arn:aws:kms::111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"an id that is no uuid": "arn:aws:kms:eu-west-1:111122223333:key/mailie",
	} {
		t.Run(name, func(t *testing.T) {
			setenv(t, map[string]string{
				"MAIL_ENV": "prod", "MAIL_CREDENTIAL_KMS_KEY_ARN": arn, "MAIL_DATA_DIR": t.TempDir(),
			})
			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "MAIL_CREDENTIAL_KMS_KEY_ARN") {
				t.Fatalf("Load = %v; want the ARN refused", err)
			}
			// One mistake, one complaint: the hex key is not asked for
			// because the KMS key is malformed.
			if strings.Contains(err.Error(), "MAIL_CREDENTIAL_KEY_HEX is required") {
				t.Errorf("a malformed KMS key also asks for the hex key:\n%v", err)
			}
		})
	}
}

func TestAKMSKeyNeedsMAIL_ENVSetAndEveryProblemIsReportedAtOnce(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KMS_KEY_ARN": "arn:aws:kms:eu-west-1:111122223333:alias/mail-credentials",
		"MAIL_LOG_LEVEL":              "chatty",
		"MAIL_DATA_DIR":               t.TempDir(),
	})
	_, err := config.Load()
	if err == nil {
		t.Fatal("Load accepted a KMS key without MAIL_ENV")
	}
	for _, want := range []string{
		"MAIL_ENV is required with MAIL_CREDENTIAL_KMS_KEY_ARN",
		"MAIL_CREDENTIAL_KMS_KEY_ARN: ",
		"an alias is not accepted",
		"MAIL_LOG_LEVEL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}

	// MAIL_ENV set to dev, its default, is a choice; unset is not.
	setenv(t, map[string]string{
		"MAIL_ENV": "dev", "MAIL_CREDENTIAL_KMS_KEY_ARN": credentialKeyARN, "MAIL_DATA_DIR": t.TempDir(),
	})
	if _, err := config.Load(); err != nil {
		t.Fatalf("MAIL_ENV=dev: %v", err)
	}
}

func TestMAIL_CREDENTIAL_SEALERKeyringBesideAKMSKeyIsTheWayBack(t *testing.T) {
	setenv(t, map[string]string{
		"MAIL_ENV":                    "prod",
		"MAIL_CREDENTIAL_KMS_KEY_ARN": credentialKeyARN,
		"MAIL_CREDENTIAL_SEALER":      "keyring",
		"MAIL_CREDENTIAL_KEY_HEX":     validKey,
		"MAIL_DATA_DIR":               t.TempDir(),
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Credentials
	if !c.KMSOpensOnly || c.Sealer() != config.SealerKeyring || c.KMSKeyARN != credentialKeyARN || len(c.Keys) != 1 {
		t.Fatalf("credentials: %+v, sealer %s", c, c.Sealer())
	}
	if out := cfg.String(); !strings.Contains(out, "credential_sealer=keyring,opens:aws-kms credential_keys=1* ") {
		t.Errorf("String():\n%s", out)
	}
}

func TestMAIL_CREDENTIAL_SEALERIsCheckedAgainstWhatIsGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want string
	}{
		"the keyring sealing beside a KMS key, without its key": {
			vars: map[string]string{
				"MAIL_ENV": "prod", "MAIL_CREDENTIAL_KMS_KEY_ARN": credentialKeyARN, "MAIL_CREDENTIAL_SEALER": "keyring",
			},
			want: "MAIL_CREDENTIAL_KEY_HEX is required with MAIL_CREDENTIAL_SEALER=keyring",
		},
		"KMS without a key": {
			vars: map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_CREDENTIAL_SEALER": "aws-kms"},
			want: "MAIL_CREDENTIAL_SEALER=aws-kms needs MAIL_CREDENTIAL_KMS_KEY_ARN",
		},
		"a sealer nobody makes": {
			vars: map[string]string{"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_CREDENTIAL_SEALER": "vault"},
			want: "MAIL_CREDENTIAL_SEALER: want",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.vars["MAIL_DATA_DIR"] = t.TempDir()
			setenv(t, tc.vars)
			if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v; want %q", err, tc.want)
			}
		})
	}

	// Said explicitly, each is what it would be by default.
	setenv(t, map[string]string{
		"MAIL_ENV": "prod", "MAIL_CREDENTIAL_KMS_KEY_ARN": credentialKeyARN, "MAIL_CREDENTIAL_SEALER": "aws-kms",
		"MAIL_DATA_DIR": t.TempDir(),
	})
	if cfg, err := config.Load(); err != nil || cfg.Credentials.Sealer() != config.SealerAWSKMS || cfg.Credentials.KMSOpensOnly {
		t.Fatalf("aws-kms said explicitly: %+v, %v", cfg.Credentials, err)
	}
	setenv(t, map[string]string{
		"MAIL_CREDENTIAL_KEY_HEX": validKey, "MAIL_CREDENTIAL_SEALER": "keyring", "MAIL_DATA_DIR": t.TempDir(),
	})
	if cfg, err := config.Load(); err != nil || cfg.Credentials.Sealer() != config.SealerKeyring || cfg.Credentials.KMSOpensOnly {
		t.Fatalf("keyring said explicitly: %+v, %v", cfg.Credentials, err)
	}
}
