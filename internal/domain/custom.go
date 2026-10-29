package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrNotVerified means the domain's DNS does not point at the platform yet.
var ErrNotVerified = errors.New("domain: the DNS for this name does not point here yet")

// ErrNoTarget means the install has not been told what custom domains point at.
var ErrNoTarget = errors.New("domain: no CNAME target is configured for this install")

// ErrDomainNotFound means the app has no such custom domain.
var ErrDomainNotFound = errors.New("domain: no such domain")

// Resolver looks up DNS. An interface so verification can be tested without a
// network, and so a caller can substitute a resolver that does not use the
// host's own cache — a freshly created record is exactly the case where a
// cached negative answer is wrong.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// NetResolver is the standard library's resolver.
type NetResolver struct{ R *net.Resolver }

func (n NetResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return n.resolver().LookupCNAME(ctx, host)
}

func (n NetResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return n.resolver().LookupHost(ctx, host)
}

func (n NetResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	return n.resolver().LookupTXT(ctx, host)
}

func (n NetResolver) resolver() *net.Resolver {
	if n.R != nil {
		return n.R
	}
	return net.DefaultResolver
}

// Custom is a hostname somebody brought, and whether it is proven.
type Custom struct {
	ID       uuid.UUID
	Host     string
	Verified bool

	// Target is what the CNAME has to point at. Carried on the row so a change
	// to the platform target shows up as a domain needing re-verification
	// rather than silently invalidating one that was proven against the old.
	Target string
}

// AddCustom claims a hostname for an app, unverified.
//
// Nothing routes to it yet. The claim is recorded so the person can be shown
// which record to create, and proving it is a separate act — see Verify.
func AddCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
	host, target, appDomain string, reserved []string,
) (Custom, error) {
	host = normalize(host)
	// A scheme is what people paste, so it is stripped rather than refused.
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")

	switch {
	case host == "":
		return Custom{}, errors.New("a domain is required, for example shop.example.com")
	case len(host) > maxHostname:
		return Custom{}, fmt.Errorf("domain: hostname exceeds %d characters", maxHostname)
	case !domainRE.MatchString(host):
		return Custom{}, fmt.Errorf("domain: %q is not a valid hostname", host)
	}

	// The platform's own domain is not somebody's to bring: a name under it is
	// issued by the platform, and claiming one here would route around the
	// uniqueness the platform relies on.
	if Reserved(host, appDomain, reserved) {
		return Custom{}, fmt.Errorf("%w: %s", ErrHostReserved, host)
	}
	if target == "" {
		return Custom{}, ErrNoTarget
	}

	row, err := q.CreateCustomDomain(ctx, dbgen.CreateCustomDomainParams{
		OwnerID: ownerID, AppID: appID, Host: host, VerifyTarget: target,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Said without saying who has it. Which team holds a name is not
			// this team's business, and the answer is the same either way.
			return Custom{}, fmt.Errorf("%w: %s", ErrHostTaken, host)
		}
		return Custom{}, fmt.Errorf("domain: claim %s: %w", host, err)
	}
	return toCustom(row), nil
}

// challengeLabel prefixes the hostname whose TXT record proves a claim.
//
// The proof lives at _ozymandis-challenge.<host> rather than on <host> itself
// so it never collides with the records that route the name, and an underscore
// label cannot be a real host, so nothing is ever served from it.
const challengeLabel = "_ozymandis-challenge."

// ChallengeHost is the record name whose TXT value proves control of host.
// Exported so the dashboard can show the operator exactly what to create.
func ChallengeHost(host string) string { return challengeLabel + normalize(host) }

// ChallengeValue is the TXT value that proves a claim: the claim's own id,
// which is unguessable and unique to this claim, so a record carrying it could
// only have been placed by whoever this claim was shown to.
func ChallengeValue(id uuid.UUID) string { return id.String() }

// Verify proves a claim by checking the claimant controls the name's DNS.
//
// Resolving to the platform is not proof: an operator's own dashboard and
// registry hostnames resolve here, and so does a domain a former tenant pointed
// here and never released — accepting either let any tenant "verify" a name it
// did not own and have an Ingress written for it. What proves control is a
// record only the zone's owner could create:
//
//   - a CNAME to the platform target. A CNAME on <host> can only be set by
//     whoever controls <host>'s DNS, and it is also what routes a subdomain
//     here, so the common case proves itself and needs nothing extra.
//
//   - for an apex, which cannot carry a CNAME, a TXT record at
//     _ozymandis-challenge.<host> holding the claim id, and the apex resolving
//     here so a proven claim also routes.
func Verify(
	ctx context.Context, q *dbgen.Queries, res Resolver, ownerID string, id uuid.UUID,
) error {
	row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDomainNotFound
		}
		return fmt.Errorf("domain: read claim: %w", err)
	}
	if row.VerifyTarget == "" {
		return ErrNoTarget
	}

	if !proven(ctx, res, row.Host, row.VerifyTarget, ChallengeValue(row.ID)) {
		return ErrNotVerified
	}

	n, err := q.VerifyCustomDomain(ctx, dbgen.VerifyCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return fmt.Errorf("domain: mark verified: %w", err)
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// proven reports whether the claimant controls host, not merely that host
// happens to point at the platform.
func proven(ctx context.Context, res Resolver, host, target, challenge string) bool {
	// The subdomain case: a CNAME to the target is itself proof of control, and
	// is the record that routes the name.
	if cname, err := res.LookupCNAME(ctx, host); err == nil {
		if normalize(cname) == normalize(target) {
			return true
		}
	}

	// The apex case: an apex cannot carry a CNAME, so control is proven by the
	// TXT challenge, and the address match the CNAME would otherwise have given
	// is still required so that a proven name also actually routes here.
	if !hasChallenge(ctx, res, host, challenge) {
		return false
	}
	return addressesOverlap(ctx, res, host, target)
}

// hasChallenge reports whether the claim's TXT challenge is published.
func hasChallenge(ctx context.Context, res Resolver, host, challenge string) bool {
	records, err := res.LookupTXT(ctx, ChallengeHost(host))
	if err != nil {
		return false
	}
	for _, r := range records {
		// A long TXT value arrives split across 255-byte strings that the
		// resolver joins; either way the comparison is on the trimmed whole.
		if strings.TrimSpace(r) == challenge {
			return true
		}
	}
	return false
}

// addressesOverlap reports whether host and target answer with a common
// address. A flattened ALIAS record looks exactly like this and is the correct
// way to point an apex at a platform.
func addressesOverlap(ctx context.Context, res Resolver, host, target string) bool {
	want, err := res.LookupHost(ctx, target)
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := res.LookupHost(ctx, host)
	if err != nil || len(got) == 0 {
		return false
	}

	set := make(map[string]bool, len(want))
	for _, a := range want {
		set[a] = true
	}
	for _, a := range got {
		if set[a] {
			return true
		}
	}
	return false
}

// ListCustom returns an app's custom domains.
func ListCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
) ([]Custom, error) {
	rows, err := q.ListCustomDomains(ctx, dbgen.ListCustomDomainsParams{
		OwnerID: ownerID, AppID: appID,
	})
	if err != nil {
		return nil, fmt.Errorf("domain: list custom: %w", err)
	}
	out := make([]Custom, 0, len(rows))
	for _, r := range rows {
		out = append(out, toCustom(r))
	}
	return out, nil
}

// RemoveCustom releases a claim.
func RemoveCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, id uuid.UUID,
) error {
	n, err := q.DeleteCustomDomain(ctx, dbgen.DeleteCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return fmt.Errorf("domain: remove custom: %w", err)
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// Routable is a hostname an app's Ingress should carry.
//
// Managed travels with the name because the two are needed together at exactly
// one place — building the Ingress — and separating them there is what served a
// customer's domain the platform's wildcard certificate.
type Routable struct {
	Host string

	// Managed is true for a hostname the platform issued, which is to say one
	// under the app domain and therefore covered by the wildcard certificate
	// the ingress controller already holds. False is a name somebody brought,
	// which that certificate does not cover.
	Managed bool
}

// RoutableHosts returns the hostnames an app's Ingress should carry.
//
// A managed host is routable because the platform issued it. A custom one only
// once it is proven — the gate is in the query rather than in a caller, so
// nothing can route an unverified claim by forgetting to check.
func RoutableHosts(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) ([]Routable, error) {
	rows, err := q.RoutableHostsForApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("domain: routable hosts: %w", err)
	}
	out := make([]Routable, 0, len(rows))
	for _, r := range rows {
		out = append(out, Routable{Host: r.Host, Managed: r.Managed})
	}
	return out, nil
}

func toCustom(row dbgen.Domain) Custom {
	return Custom{
		ID: row.ID, Host: row.Host, Verified: row.Verified, Target: row.VerifyTarget,
	}
}
