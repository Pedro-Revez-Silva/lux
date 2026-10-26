package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

const (
	// ecrOwnerTag is the session tag the pull role's policy scopes
	// repositories by (repository/${aws:PrincipalTag/lux-owner}/*).
	ecrOwnerTag      = "lux-owner"
	ecrSessionPrefix = "luxd-registry-"
	ecrSession       = time.Hour
	// ecrMaxOwner: STS session names are at most 64 characters, and the
	// session is named ecrSessionPrefix + owner.
	ecrMaxOwner = 64 - len(ecrSessionPrefix)
)

var (
	// ecrHostRe: a private ECR registry, <account>.dkr.ecr.<region>.amazonaws.com
	// (.com.cn in China); its region is the first capture.
	ecrHostRe = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
	ecrRoleRe = regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
)

// ECRConfig is [registry.ecr].
type ECRConfig struct {
	Region      string
	PullRoleARN string
}

// Problems is what makes c unusable for the registry host and owners, each
// naming its TOML key and variable.
func (c ECRConfig) Problems(host string, owners []string) []string {
	var problems []string
	m := ecrHostRe.FindStringSubmatch(host)
	if m == nil {
		problems = append(problems, fmt.Sprintf("registry.host (LUX_REGISTRY_HOST) %q: provider ecr wants <account>.dkr.ecr.<region>.amazonaws.com", host))
	}
	switch {
	case c.Region == "":
		problems = append(problems, "registry.ecr.region (LUX_REGISTRY_ECR_REGION) is required")
	case m != nil && m[1] != c.Region:
		problems = append(problems, fmt.Sprintf("registry.ecr.region (LUX_REGISTRY_ECR_REGION) %q: registry.host is in %s", c.Region, m[1]))
	}
	if !ecrRoleRe.MatchString(c.PullRoleARN) {
		problems = append(problems, fmt.Sprintf("registry.ecr.pull_role_arn (LUX_REGISTRY_ECR_PULL_ROLE_ARN) %q: want arn:<partition>:iam::<account>:role/<name>", c.PullRoleARN))
	}
	for i, owner := range owners {
		if len(owner) > ecrMaxOwner {
			problems = append(problems, fmt.Sprintf("registry.grants[%d].owner %q: provider ecr allows at most %d characters", i, owner, ecrMaxOwner))
		}
	}
	return problems
}

// ECRSource mints ECR pull tokens: STS AssumeRole of the pull role with
// luxd's own credentials and the session tag lux-owner=<owner>, then ECR
// GetAuthorizationToken with the assumed ones. The pull role's policy
// limits the token to repository/<owner>/* (docs/operations.md).
type ECRSource struct {
	roleARN string
	api     ecrAPI
}

// NewECRSource is an ECRSource on the AWS SDK and luxd's default
// credential chain, loaded on first use.
func NewECRSource(c ECRConfig) *ECRSource {
	return &ECRSource{roleARN: c.PullRoleARN, api: &awsECR{region: c.Region}}
}

// ecrAPI is the two AWS calls a mint takes.
type ecrAPI interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error)
	AuthorizationToken(ctx context.Context, creds *ststypes.Credentials) (*ecrtypes.AuthorizationData, error)
}

func (s *ECRSource) Mint(ctx context.Context, owner string) (RegistryToken, error) {
	creds, err := s.api.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(s.roleARN),
		RoleSessionName: aws.String(ecrSessionPrefix + owner),
		DurationSeconds: aws.Int32(int32(ecrSession / time.Second)),
		Tags:            []ststypes.Tag{{Key: aws.String(ecrOwnerTag), Value: aws.String(owner)}},
	})
	if err != nil {
		return RegistryToken{}, fmt.Errorf("sts:AssumeRole: %w", err)
	}
	if creds == nil {
		return RegistryToken{}, errors.New("sts:AssumeRole: no credentials")
	}
	d, err := s.api.AuthorizationToken(ctx, creds)
	if err != nil {
		return RegistryToken{}, fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}
	if d == nil || d.AuthorizationToken == nil || d.ExpiresAt == nil {
		return RegistryToken{}, errors.New("ecr:GetAuthorizationToken: no token")
	}
	// The errors below never quote the token: it may be a valid one.
	raw, err := base64.StdEncoding.DecodeString(*d.AuthorizationToken)
	if err != nil {
		return RegistryToken{}, errors.New("ecr:GetAuthorizationToken: token is not base64")
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user != "AWS" || pass == "" {
		return RegistryToken{}, errors.New("ecr:GetAuthorizationToken: token is not AWS:password")
	}
	// A token is presumed to last no longer than the session that minted it.
	expires := *d.ExpiresAt
	if creds.Expiration != nil && creds.Expiration.Before(expires) {
		expires = *creds.Expiration
	}
	return RegistryToken{User: user, Password: pass, Expires: expires}, nil
}

type awsECR struct {
	region string
	once   sync.Once
	cfg    aws.Config
	err    error
}

func (r *awsECR) load(ctx context.Context) (aws.Config, error) {
	r.once.Do(func() { r.cfg, r.err = config.LoadDefaultConfig(ctx, config.WithRegion(r.region)) })
	return r.cfg, r.err
}

func (r *awsECR) AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error) {
	cfg, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	out, err := sts.NewFromConfig(cfg).AssumeRole(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

func (r *awsECR) AuthorizationToken(ctx context.Context, c *ststypes.Credentials) (*ecrtypes.AuthorizationData, error) {
	cfg, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	out, err := ecr.NewFromConfig(cfg, func(o *ecr.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider(aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken))
	}).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, err
	}
	if len(out.AuthorizationData) == 0 {
		return nil, errors.New("no authorization data")
	}
	return &out.AuthorizationData[0], nil
}
