package prometheus

import (
	"context"
	"fmt"
	"io"

	"strings"

	prommodel "github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/rulefmt"
	"gopkg.in/yaml.v2"

	"github.com/slok/sloth/internal/info"
	"github.com/slok/sloth/internal/log"
)

type OutputFlavor int

const (
	PrometheusFlavor OutputFlavor = iota
	ChronosphereFlavor
)

var (
	// ErrNoSLORules will be used when there are no rules to store. The upper layer
	// could ignore or handle the error in cases where there wasn't an output.
	ErrNoSLORules = fmt.Errorf("0 SLO Prometheus rules generated")
)

func NewIOWriterGroupedRulesYAMLRepo(writer io.Writer, logger log.Logger) IOWriterGroupedRulesYAMLRepo {
	return IOWriterGroupedRulesYAMLRepo{
		writer: writer,
		logger: logger.WithValues(log.Kv{"svc": "storage.IOWriter", "format": "yaml"}),
	}
}

// IOWriterGroupedRulesYAMLRepo knows to store all the SLO rules (recordings and alerts)
// grouped in an IOWriter in YAML format, that is compatible with Prometheus.
type IOWriterGroupedRulesYAMLRepo struct {
	writer io.Writer
	logger log.Logger
}

type StorageSLO struct {
	SLO   SLO
	Rules SLORules
}

// StoreSLOs will store the recording and alert prometheus rules, if grouped is false it will
// split and store as 2 different groups the alerts and the recordings, if true
// it will be save as a single group.
func (i IOWriterGroupedRulesYAMLRepo) StoreSLOs(ctx context.Context, slos []StorageSLO, flavor OutputFlavor) error {
	if len(slos) == 0 {
		return fmt.Errorf("slo rules required")
	}

	// If we don't have anything to store, error so we can increase the reliability
	// because maybe this was due to an unintended error (typos, misconfig, too many disable...).
	rules := 0
	err := error(nil)
	var rulesYaml []byte

	logger := i.logger.WithCtxValues(ctx)

	if flavor == PrometheusFlavor {
		// Convert to YAML (Prometheus rule format).
		rules, rulesYaml, err = rawPrometheusYAML(slos, logger)

		if err != nil {
			return err
		}

	} else if flavor == ChronosphereFlavor {
		rules, rulesYaml, err = rawChronosphereYAML(slos, logger)

		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unsupported flavor")
	}

	rulesYaml = writeTopDisclaimer(rulesYaml)
	_, err = i.writer.Write(rulesYaml)
	if err != nil {
		return fmt.Errorf("could not write top disclaimer: %w", err)
	}

	logger.WithValues(log.Kv{"groups": rules}).Infof("Prometheus rules written")

	return nil
}
func rawChronosphereYAML(slos []StorageSLO, logger log.Logger) (int, []byte, error) {
	collections := make(map[string]chronosphereCollection)
	rules := []chronosphereRule{}

	for _, slo := range slos {
		collection := chronosphereCollection{
			Slug:        fmt.Sprintf("sloth-slo-%s", slo.SLO.Service),
			Name:        fmt.Sprintf("sloth-slo-%s", slo.SLO.Service),
			Description: "SLOs generated by Sloth",
		}

		for _, rule := range slo.Rules.SLIErrorRecRules {
			ruleId := fmt.Sprintf("sloth-slo-sli-recordings-%s-%s", slo.SLO.ID, strings.Replace(rule.Record, ":", "_", -1))
			chronoRule := chronosphereRule{
				Slug:          ruleId,
				Name:          ruleId,
				Collection:    collection.Slug,
				Interval_secs: 300,
				Metric_name:   rule.Record,
				Expr:          rule.Expr,
				Label_policy: chronosphereLabelPolicy{
					Add: rule.Labels,
				},
			}
			rules = append(rules, chronoRule)
		}

		for _, rule := range slo.Rules.MetadataRecRules {
			ruleId := fmt.Sprintf("sloth-slo-sli-recordings-%s-%s", slo.SLO.ID, strings.Replace(rule.Record, ":", "_", -1))
			chronoRule := chronosphereRule{
				Slug:          ruleId,
				Name:          ruleId,
				Collection:    collection.Slug,
				Interval_secs: 300,
				Metric_name:   rule.Record,
				Expr:          rule.Expr,
				Label_policy: chronosphereLabelPolicy{
					Add: rule.Labels,
				},
			}
			rules = append(rules, chronoRule)
		}
		collections[collection.Slug] = collection

	}

	if len(collections) == 0 {
		return 0, nil, ErrNoSLORules
	}

	outputYaml := make([]byte, 0)

	for _, collection := range collections {
		chronosphereCollectionYAML := NewChronosphereCollectionYAML()
		chronosphereCollectionYAML.Spec = collection
		collectionYaml, err := yaml.Marshal(chronosphereCollectionYAML)
		if err != nil {
			return 0, nil, fmt.Errorf("could not format collections: %w", err)
		}
		outputYaml = append(outputYaml, collectionYaml...)
		outputYaml = append(outputYaml, []byte("---\n")...)
	}

	for _, rule := range rules {
		chronosphereRuleYAML := NewChronosphereRuleYAML()
		chronosphereRuleYAML.Spec = rule
		ruleYaml, err := yaml.Marshal(chronosphereRuleYAML)
		if err != nil {
			return 0, nil, fmt.Errorf("could not format rules: %w", err)
		}
		outputYaml = append(outputYaml, ruleYaml...)
		outputYaml = append(outputYaml, []byte("---\n")...)
	}

	return len(collections), outputYaml, nil
}

func rawPrometheusYAML(slos []StorageSLO, logger log.Logger) (int, []byte, error) {
	ruleGroups := ruleGroupsYAMLv2{}
	for _, slo := range slos {
		if len(slo.Rules.SLIErrorRecRules) > 0 {
			ruleGroups.Groups = append(ruleGroups.Groups, ruleGroupYAMLv2{
				Name:  fmt.Sprintf("sloth-slo-sli-recordings-%s", slo.SLO.ID),
				Rules: slo.Rules.SLIErrorRecRules,
			})
		}

		if len(slo.Rules.MetadataRecRules) > 0 {
			ruleGroups.Groups = append(ruleGroups.Groups, ruleGroupYAMLv2{
				Name:  fmt.Sprintf("sloth-slo-meta-recordings-%s", slo.SLO.ID),
				Rules: slo.Rules.MetadataRecRules,
			})
		}

		if len(slo.Rules.AlertRules) > 0 {
			ruleGroups.Groups = append(ruleGroups.Groups, ruleGroupYAMLv2{
				Name:  fmt.Sprintf("sloth-slo-alerts-%s", slo.SLO.ID),
				Rules: slo.Rules.AlertRules,
			})
		}
	}

	if len(ruleGroups.Groups) == 0 {
		return 0, nil, ErrNoSLORules
	}

	rulesYaml, err := yaml.Marshal(ruleGroups)
	if err != nil {
		return 0, nil, fmt.Errorf("could not format rules: %w", err)
	}
	return len(ruleGroups.Groups), rulesYaml, err
}

var disclaimer = fmt.Sprintf(`
---
# Code generated by Sloth (%s): https://github.com/slok/sloth.
# DO NOT EDIT.

`, info.Version)

func writeTopDisclaimer(bs []byte) []byte {
	return append([]byte(disclaimer), bs...)
}

// these types are defined to support yaml v2 (instead of the new Prometheus
// YAML v3 that has some problems with marshaling).
type ruleGroupsYAMLv2 struct {
	Groups []ruleGroupYAMLv2 `yaml:"groups"`
}

type ruleGroupYAMLv2 struct {
	Name     string             `yaml:"name"`
	Interval prommodel.Duration `yaml:"interval,omitempty"`
	Rules    []rulefmt.Rule     `yaml:"rules"`
}

type chronosphereCollectionYAML struct {
	Api_version string                 `yaml:"api_version"`
	Kind        string                 `yaml:"kind"`
	Spec        chronosphereCollection `yaml:"spec"`
}

func NewChronosphereCollectionYAML() chronosphereCollectionYAML {
	return chronosphereCollectionYAML{
		Api_version: "v1/config",
		Kind:        "Collection",
	}
}

type chronosphereCollection struct {
	Slug                     string `yaml:"slug"`
	Name                     string `yaml:"name"`
	Description              string `yaml:"description"`
	Team_slug                string `yaml:"team_slug,omitempty"`
	Notification_policy_slug string `yaml:"notification_policy_slug,omitempty"`
}

type chronosphereLabelPolicy struct {
	Add map[string]string `yaml:"add"`
}

type chronosphereRuleYAML struct {
	Api_version string           `yaml:"api_version"`
	Kind        string           `yaml:"kind"`
	Spec        chronosphereRule `yaml:"spec"`
}

func NewChronosphereRuleYAML() chronosphereRuleYAML {
	return chronosphereRuleYAML{
		Api_version: "v1/config",
		Kind:        "RecordingRule",
	}
}

type chronosphereRule struct {
	Slug          string                  `yaml:"slug"`
	Name          string                  `yaml:"name"`
	Collection    string                  `yaml:"bucket_slug"`
	Interval_secs int                     `yaml:"interval_secs"`
	Metric_name   string                  `yaml:"metric_name"`
	Expr          string                  `yaml:"prometheus_expr"`
	Label_policy  chronosphereLabelPolicy `yaml:"label_policy"`
}
