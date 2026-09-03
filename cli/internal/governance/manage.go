package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kcrmin/Stackcord/cli/internal/domain"
	"github.com/kcrmin/Stackcord/cli/internal/operation"
	"github.com/kcrmin/Stackcord/cli/internal/schema"
	"github.com/kcrmin/Stackcord/cli/internal/workspace"
	"go.yaml.in/yaml/v3"
)

type AuthorityAction string

const (
	AuthorityAdd    AuthorityAction = "add"
	AuthorityRemove AuthorityAction = "remove"
)

var authoritySubjectPattern = regexp.MustCompile(`^(?:user|team):[A-Za-z0-9][A-Za-z0-9._/-]*$`)

type AuthorityChangeRequest struct {
	Root                      string
	Action                    AuthorityAction
	Subject                   string
	ExpectedPolicyFingerprint string
}

type AuthorityChange struct {
	Action            AuthorityAction `json:"action"`
	Subject           string          `json:"subject"`
	PolicyFingerprint string          `json:"policy_fingerprint"`
	Before            []string        `json:"before"`
	After             []string        `json:"after"`
}

// PlanAuthorityChange prepares one exact governance-policy proposal without granting approval.
func PlanAuthorityChange(ctx context.Context, request AuthorityChangeRequest) (AuthorityChange, operation.Plan, error) {
	located, err := workspace.FindRoot(ctx, request.Root)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	request.Root = located.Path
	request.Subject = strings.TrimSpace(request.Subject)
	plan := operation.Plan{ID: authorityOperationID(request.Action, request.Subject, ""), Root: request.Root}
	change := AuthorityChange{Action: request.Action, Subject: request.Subject, Before: []string{}, After: []string{}}

	policy, err := LoadPolicy(request.Root)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	path := filepath.Join(request.Root, ".harness", "governance.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	change.PolicyFingerprint = policyDigest(raw)
	plan.ID = authorityOperationID(request.Action, request.Subject, change.PolicyFingerprint)
	change.Before = append([]string(nil), policy.ProductAuthorities...)
	change.After = append([]string(nil), change.Before...)

	block := func(code, message string, refs ...string) (AuthorityChange, operation.Plan, error) {
		plan.Files = nil
		plan.Blockers = append(plan.Blockers, domain.Item{Code: code, Message: message, Refs: refs})
		return change, plan, nil
	}
	if request.ExpectedPolicyFingerprint != "" && request.ExpectedPolicyFingerprint != change.PolicyFingerprint {
		return block("governance.policy-stale", "Governance policy changed after the authority plan was reviewed.", request.ExpectedPolicyFingerprint, change.PolicyFingerprint)
	}
	if !policy.Enabled {
		return block("governance.disabled", "Product governance must be enabled before authorities can be changed.")
	}
	if !authoritySubjectPattern.MatchString(request.Subject) {
		return block("governance.authority-invalid", "Product authority must use a normalized user: or team: subject.", request.Subject)
	}

	current := make(map[string]bool, len(change.Before))
	for _, subject := range change.Before {
		current[subject] = true
	}
	switch request.Action {
	case AuthorityAdd:
		if current[request.Subject] {
			return block("governance.authority-exists", "Product authority is already configured.", request.Subject)
		}
		change.After = append(change.After, request.Subject)
	case AuthorityRemove:
		if !current[request.Subject] {
			return block("governance.authority-missing", "Product authority is not configured.", request.Subject)
		}
		if len(change.Before) == 1 {
			return block("governance.authority-lockout", "The final product authority cannot be removed while governance is enabled.", request.Subject)
		}
		change.After = change.After[:0]
		for _, subject := range change.Before {
			if subject != request.Subject {
				change.After = append(change.After, subject)
			}
		}
		if policy.Approval.Minimum > len(change.After) {
			return block("governance.approval-lockout", "Removing this authority would make the configured approval minimum impossible.", request.Subject)
		}
	default:
		return block("governance.authority-action-invalid", "Authority action must be add or remove.", string(request.Action))
	}
	sort.Strings(change.After)

	content, err := replaceAuthorities(raw, change.After)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	proposed, err := schema.DecodeYAML[Policy](content)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, fmt.Errorf("decode proposed governance policy: %w", err)
	}
	if issues := schema.Validate("governance", proposed); len(issues) > 0 {
		return AuthorityChange{}, operation.Plan{}, fmt.Errorf("validate proposed governance policy: %s", issues[0].Message)
	}
	info, err := os.Stat(path)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	plan.Files = []operation.FileChange{{Path: filepath.ToSlash(filepath.Join(".harness", "governance.yaml")), Content: content, Mode: info.Mode().Perm()}}
	plan.InitialStateFingerprint, err = operation.StateFingerprint(plan)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	currentRaw, err := os.ReadFile(path)
	if err != nil {
		return AuthorityChange{}, operation.Plan{}, err
	}
	if policyDigest(currentRaw) != change.PolicyFingerprint {
		return block("governance.policy-stale", "Governance policy changed while the authority plan was being created.", change.PolicyFingerprint, policyDigest(currentRaw))
	}
	return change, plan, nil
}

func replaceAuthorities(raw []byte, authorities []string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("governance policy must be one YAML mapping")
	}
	mapping := document.Content[0]
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != "product_authorities" {
			continue
		}
		sequence := mapping.Content[index+1]
		style := sequence.Style
		existing := make(map[string]*yaml.Node, len(sequence.Content))
		for _, item := range sequence.Content {
			if item.Kind == yaml.ScalarNode {
				existing[item.Value] = item
			}
		}
		sequence.Kind = yaml.SequenceNode
		sequence.Tag = "!!seq"
		sequence.Style = style
		sequence.Content = nil
		for _, subject := range authorities {
			item := existing[subject]
			if item == nil {
				item = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: subject}
			}
			sequence.Content = append(sequence.Content, item)
		}
		return yaml.Marshal(&document)
	}
	return nil, fmt.Errorf("governance policy has no product_authorities field")
}

func authorityOperationID(action AuthorityAction, subject, policyFingerprint string) string {
	sum := sha256.Sum256([]byte(string(action) + "\x00" + subject + "\x00" + policyFingerprint))
	return "governance-authority-" + string(action) + "-" + hex.EncodeToString(sum[:6])
}

func policyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
