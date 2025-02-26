// Copyright 2022 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package server

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	ctclient "github.com/google/certificate-transparency-go/client"
	certauth "github.com/sigstore/fulcio/pkg/ca"
	"github.com/sigstore/fulcio/pkg/challenges"
	"github.com/sigstore/fulcio/pkg/config"
	"github.com/sigstore/fulcio/pkg/ctl"
	fulciogrpc "github.com/sigstore/fulcio/pkg/generated/protobuf"
	"github.com/sigstore/fulcio/pkg/log"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

type grpcCAServer struct {
	fulciogrpc.UnimplementedCAServer
	ct *ctclient.LogClient
	ca certauth.CertificateAuthority
}

func NewGRPCCAServer(ct *ctclient.LogClient, ca certauth.CertificateAuthority) fulciogrpc.CAServer {
	return &grpcCAServer{
		ct: ct,
		ca: ca,
	}
}

const (
	MetadataOIDCTokenKey = "oidcidentitytoken"
)

func (g *grpcCAServer) CreateSigningCertificate(ctx context.Context, request *fulciogrpc.CreateSigningCertificateRequest) (*fulciogrpc.SigningCertificate, error) {
	logger := log.ContextLogger(ctx)

	// OIDC token either is passed in gRPC field or was extracted from HTTP headers
	token := ""
	if request.Credentials != nil {
		token = request.Credentials.GetOidcIdentityToken()
	}
	if token == "" {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			vals := md.Get(MetadataOIDCTokenKey)
			if len(vals) == 1 {
				token = vals[0]
			}
		}
	}

	// Authenticate OIDC ID token by checking signature
	idtoken, err := authorize(ctx, token)
	if err != nil {
		return nil, handleFulcioGRPCError(ctx, codes.Unauthenticated, err, invalidCredentials)
	}
	// Parse authenticated ID token into principal
	// TODO:(nsmith5) replace this and authorize call above with
	// just identity.IssuerPool.Authenticate()
	principal, err := challenges.PrincipalFromIDToken(ctx, idtoken)
	if err != nil {
		return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, invalidIdentityToken)
	}

	var publicKey crypto.PublicKey
	// Verify caller is in possession of their private key and extract
	// public key from request.
	if len(request.GetCertificateSigningRequest()) > 0 {
		// Option 1: Verify CSR
		csr, err := cryptoutils.ParseCSR(request.GetCertificateSigningRequest())
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, invalidCSR)
		}

		// Parse public key and check for weak key parameters
		publicKey = csr.PublicKey
		if err := cryptoutils.ValidatePubKey(publicKey); err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, insecurePublicKey)
		}

		if err := csr.CheckSignature(); err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, invalidSignature)
		}
	} else {
		// Option 2: Check the signature for proof of possession of a private key
		var (
			pubKeyContent     string
			proofOfPossession []byte
			err               error
		)
		if request.GetPublicKeyRequest() != nil {
			if request.GetPublicKeyRequest().PublicKey != nil {
				pubKeyContent = request.GetPublicKeyRequest().PublicKey.Content
			}
			proofOfPossession = request.GetPublicKeyRequest().ProofOfPossession
		}

		// Parse public key and check for weak parameters
		publicKey, err = challenges.ParsePublicKey(pubKeyContent)
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, invalidPublicKey)
		}
		if err := cryptoutils.ValidatePubKey(publicKey); err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, insecurePublicKey)
		}

		// Check proof of possession signature
		if err := challenges.CheckSignature(publicKey, proofOfPossession, principal.Name(ctx)); err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, invalidSignature)
		}
	}

	var csc *certauth.CodeSigningCertificate
	var sctBytes []byte
	result := &fulciogrpc.SigningCertificate{}
	// For CAs that do not support embedded SCTs or if the CT log is not configured
	if sctCa, ok := g.ca.(certauth.EmbeddedSCTCA); !ok || g.ct == nil {
		// currently configured CA doesn't support pre-certificate flow required to embed SCT in final certificate
		csc, err = g.ca.CreateCertificate(ctx, principal, publicKey)
		if err != nil {
			// if the error was due to invalid input in the request, return HTTP 400
			if _, ok := err.(certauth.ValidationError); ok {
				return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, err.Error())
			}
			// otherwise return a 500 error to reflect that it is a transient server issue that the client can't resolve
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
		}

		// Submit to CTL
		if g.ct != nil {
			sct, err := g.ct.AddChain(ctx, ctl.BuildCTChain(csc.FinalCertificate, csc.FinalChain))
			if err != nil {
				return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToEnterCertInCTL)
			}
			// convert to AddChainResponse because Cosign expects this struct.
			addChainResp, err := ctl.ToAddChainResponse(sct)
			if err != nil {
				return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalSCT)
			}
			sctBytes, err = json.Marshal(addChainResp)
			if err != nil {
				return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalSCT)
			}
		} else {
			logger.Info("Skipping CT log upload.")
		}

		finalPEM, err := csc.CertPEM()
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalCert)
		}

		finalChainPEM, err := csc.ChainPEM()
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalCert)
		}

		result.Certificate = &fulciogrpc.SigningCertificate_SignedCertificateDetachedSct{
			SignedCertificateDetachedSct: &fulciogrpc.SigningCertificateDetachedSCT{
				Chain: &fulciogrpc.CertificateChain{
					Certificates: append([]string{finalPEM}, finalChainPEM...),
				},
			},
		}
		if len(sctBytes) > 0 {
			result.GetSignedCertificateDetachedSct().SignedCertificateTimestamp = sctBytes
		}
	} else {
		precert, err := sctCa.CreatePrecertificate(ctx, principal, publicKey)
		if err != nil {
			// if the error was due to invalid input in the request, return HTTP 400
			if _, ok := err.(certauth.ValidationError); ok {
				return nil, handleFulcioGRPCError(ctx, codes.InvalidArgument, err, err.Error())
			}
			// otherwise return a 500 error to reflect that it is a transient server issue that the client can't resolve
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
		}
		// submit precertificate and chain to CT log
		sct, err := g.ct.AddPreChain(ctx, ctl.BuildCTChain(precert.PreCert, precert.CertChain))
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToEnterCertInCTL)
		}
		csc, err = sctCa.IssueFinalCertificate(ctx, precert, sct)
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
		}

		finalPEM, err := csc.CertPEM()
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalCert)
		}

		finalChainPEM, err := csc.ChainPEM()
		if err != nil {
			return nil, handleFulcioGRPCError(ctx, codes.Internal, err, failedToMarshalCert)
		}

		result.Certificate = &fulciogrpc.SigningCertificate_SignedCertificateEmbeddedSct{
			SignedCertificateEmbeddedSct: &fulciogrpc.SigningCertificateEmbeddedSCT{
				Chain: &fulciogrpc.CertificateChain{
					Certificates: append([]string{finalPEM}, finalChainPEM...),
				},
			},
		}
	}

	metricNewEntries.Inc()

	return result, nil
}

func (g *grpcCAServer) GetTrustBundle(ctx context.Context, _ *fulciogrpc.GetTrustBundleRequest) (*fulciogrpc.TrustBundle, error) {
	logger := log.ContextLogger(ctx)

	trustBundle, err := g.ca.TrustBundle(ctx)
	if err != nil {
		logger.Error("Error retrieving trust bundle: ", err)
		return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
	}

	resp := &fulciogrpc.TrustBundle{
		Chains: []*fulciogrpc.CertificateChain{},
	}

	for _, chain := range trustBundle {
		certChain := &fulciogrpc.CertificateChain{}
		for _, cert := range chain {
			certPEM, err := cryptoutils.MarshalCertificateToPEM(cert)
			if err != nil {
				return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
			}
			certChain.Certificates = append(certChain.Certificates, string(certPEM))
		}
		resp.Chains = append(resp.Chains, certChain)
	}
	return resp, nil
}

func (g *grpcCAServer) GetConfiguration(ctx context.Context, _ *fulciogrpc.GetConfigurationRequest) (*fulciogrpc.Configuration, error) {
	logger := log.ContextLogger(ctx)

	cfg := config.FromContext(ctx)
	if cfg == nil {
		err := errors.New("configuration not loaded")
		logger.Error(err)
		return nil, handleFulcioGRPCError(ctx, codes.Internal, err, genericCAError)
	}

	return &fulciogrpc.Configuration{
		Issuers: cfg.ToIssuers(),
	}, nil
}

func extractIssuer(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("oidc: malformed jwt, expected 3 parts got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("oidc: malformed jwt payload: %w", err)
	}
	var payload struct {
		Issuer string `json:"iss"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("oidc: failed to unmarshal claims: %w", err)
	}
	return payload.Issuer, nil
}

// We do this to bypass needing actual OIDC tokens for unit testing.
var authorize = actualAuthorize

func actualAuthorize(ctx context.Context, token string) (*oidc.IDToken, error) {
	issuer, err := extractIssuer(token)
	if err != nil {
		return nil, err
	}

	verifier, ok := config.FromContext(ctx).GetVerifier(issuer)
	if !ok {
		return nil, fmt.Errorf("unsupported issuer: %s", issuer)
	}
	return verifier.Verify(ctx, token)
}
