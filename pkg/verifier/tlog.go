package verifier

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	rekorClient "github.com/sigstore/rekor/pkg/client"
	rekorGeneratedClient "github.com/sigstore/rekor/pkg/generated/client"
	rekorEntries "github.com/sigstore/rekor/pkg/generated/client/entries"
	rekorModels "github.com/sigstore/rekor/pkg/generated/models"
	rekorVerify "github.com/sigstore/rekor/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/github/sigstore-verifier/pkg/root"
	"github.com/github/sigstore-verifier/pkg/tlog"
)

type ArtifactTransparencyLogVerifier struct {
	trustedRoot   root.TrustedRoot
	threshold     int
	online        bool
	tlogVerifiers map[string]*root.TlogVerifier
}

func (p *ArtifactTransparencyLogVerifier) Verify(entity SignedEntity) error {
	entries, err := entity.TlogEntries()
	if err != nil {
		return err
	}
	if len(entries) < p.threshold {
		return fmt.Errorf("not enough transparency log entries: %d < %d", len(entries), p.threshold)
	}

	sigContent, err := entity.SignatureContent()
	if err != nil {
		return err
	}

	entitySignature := sigContent.GetSignature()

	verificationContent, err := entity.VerificationContent()
	if err != nil {
		return err
	}

	for _, entry := range entries {
		err := tlog.ValidateEntry(entry)
		if err != nil {
			return err
		}

		if !p.online {
			err = tlog.VerifySET(entry, p.trustedRoot.TlogVerifiers())
			if err != nil {
				return err
			}
		} else {
			keyID := entry.LogKeyID()
			hex64Key := hex.EncodeToString([]byte(*keyID))
			tlogVerifier, ok := p.tlogVerifiers[hex64Key]
			if !ok {
				return fmt.Errorf("unable to find tlog information for key %s", hex64Key)
			}

			client, verifier, err := getRekorClient(tlogVerifier.BaseURL)
			if err != nil {
				return err
			}

			logIndex := entry.LogIndex()

			searchParams := rekorEntries.NewSearchLogQueryParams()
			searchLogQuery := rekorModels.SearchLogQuery{}
			searchLogQuery.LogIndexes = []*int64{logIndex}
			searchParams.SetEntry(&searchLogQuery)

			resp, err := client.Entries.SearchLogQuery(searchParams)
			if err != nil {
				return err
			}

			if len(resp.Payload) == 0 {
				return fmt.Errorf("unable to locate log entry %d", *logIndex)
			} else if len(resp.Payload) > 1 {
				return errors.New("too many log entries returned")
			}

			logEntry := resp.Payload[0]

			for _, v := range logEntry {
				v := v
				err = rekorVerify.VerifyLogEntry(context.TODO(), &v, *verifier)
				if err != nil {
					return err
				}
			}
		}

		// Ensure entry signature matches signature from bundle
		if !bytes.Equal(entry.Signature(), entitySignature) {
			return errors.New("transparency log signature does not match")
		}

		// Ensure entry certificate matches bundle certificate
		if !verificationContent.CompareKey(entry.Certificate()) {
			return errors.New("transparency log certificate does not match")
		}

		// TODO: if you have access to artifact, check that it matches body subject

		// Check tlog entry time against bundle certificates
		if !verificationContent.ValidAtTime(entry.IntegratedTime()) {
			return errors.New("Integrated time outside certificate validity")
		}
	}

	return nil
}

func NewArtifactTransparencyLogVerifier(trustedRoot root.TrustedRoot, threshold int, online bool, tlogVerifiers map[string]*root.TlogVerifier) *ArtifactTransparencyLogVerifier {
	return &ArtifactTransparencyLogVerifier{
		trustedRoot:   trustedRoot,
		threshold:     threshold,
		online:        online,
		tlogVerifiers: tlogVerifiers,
	}
}

func getRekorClient(baseURL string) (*rekorGeneratedClient.Rekor, *signature.Verifier, error) {
	client, err := rekorClient.GetRekorClient(baseURL)
	if err != nil {
		return nil, nil, err
	}

	keyResp, err := client.Pubkey.GetPublicKey(nil)
	if err != nil {
		return nil, nil, err
	}

	block, _ := pem.Decode([]byte(keyResp.Payload))
	if block == nil {
		return nil, nil, errors.New("failed to decode public key of server")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	verifier, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return nil, nil, err
	}

	return client, &verifier, nil
}