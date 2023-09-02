package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/log"
	legacymodels "github.com/grafana/grafana/pkg/services/alerting/models"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/folder"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/tsdb/graphite"
	"github.com/grafana/grafana/pkg/util"
)

const (
	// ContactLabel is a private label created during migration and used in notification policies.
	// It stores a string array of all contact point names an alert rule should send to.
	// It was created as a means to simplify post-migration notification policies.
	ContactLabel = "__contacts__"
)

// migrateAlert migrates a single dashboard alert from legacy alerting to unified alerting.
func (om *orgMigration) migrateAlert(ctx context.Context, l log.Logger, da dashAlert, dash *dashboards.Dashboard, f *folder.Folder) (*ngmodels.AlertRule, []uidOrID, error) {
	l.Debug("migrating alert rule to Unified Alerting")
	var parsedSettings dashAlertSettings
	err := json.Unmarshal(da.Settings, &parsedSettings)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse settings: %w", err)
	}
	newCond, err := transConditions(ctx, parsedSettings, da.OrgId, om.dsCacheService)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to transform conditions: %w", err)
	}

	rule, err := om.makeAlertRule(l, *newCond, da, dash, f.UID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to make alert rule: %w", err)
	}

	return rule, extractChannelIDs(parsedSettings), nil
}

func addMigrationInfo(da *dashAlert, dashboardUID string) (map[string]string, map[string]string) {
	tagsMap := simplejson.NewFromAny(da.ParsedSettings.AlertRuleTags).MustMap()
	lbls := make(map[string]string, len(tagsMap))

	for k, v := range tagsMap {
		lbls[k] = simplejson.NewFromAny(v).MustString()
	}

	annotations := make(map[string]string, 3)
	annotations[ngmodels.DashboardUIDAnnotation] = dashboardUID
	annotations[ngmodels.PanelIDAnnotation] = fmt.Sprintf("%v", da.PanelId)
	annotations["__alertId__"] = fmt.Sprintf("%v", da.Id)

	return lbls, annotations
}

// makeAlertRule creates an alert rule from a dashboard alert and the given translated condition.
func (om *orgMigration) makeAlertRule(l log.Logger, cond condition, da dashAlert, dash *dashboards.Dashboard, folderUID string) (*ngmodels.AlertRule, error) {
	lbls, annotations := addMigrationInfo(&da, dash.UID)
	annotations["message"] = da.Message
	var err error

	data, err := migrateAlertRuleQueries(l, cond.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate alert rule queries: %w", err)
	}

	// Here we ensure that the alert rule title is unique within the folder.
	if _, ok := om.alertRuleTitleDedup[folderUID]; !ok {
		om.alertRuleTitleDedup[folderUID] = deduplicator{
			set:             make(map[string]struct{}),
			caseInsensitive: om.dialect.SupportEngine(),
			maxLen:          store.AlertDefinitionMaxTitleLength,
		}
	}
	dedupSet := om.alertRuleTitleDedup[folderUID]
	name := truncateRuleName(da.Name)
	if dedupSet.contains(name) {
		dedupedName, err := dedupSet.deduplicate(name)
		if err != nil {
			return nil, fmt.Errorf("failed to deduplicate alert rule name: %w", err)
		}
		om.log.Warn("duplicate alert rule name detected, renaming", "old_name", name, "new_name", dedupedName)
		name = dedupedName
	}
	dedupSet.add(name)

	isPaused := false
	if da.State == "paused" {
		isPaused = true
	}

	ar := &ngmodels.AlertRule{
		OrgID:           da.OrgId,
		Title:           name,
		UID:             util.GenerateShortUID(),
		Condition:       cond.Condition,
		Data:            data,
		IntervalSeconds: ruleAdjustInterval(da.Frequency),
		Version:         1,
		NamespaceUID:    folderUID,
		DashboardUID:    &dash.UID,
		PanelID:         &da.PanelId,
		RuleGroup:       fmt.Sprintf("%s - %d", dash.Title, da.PanelId), // Unique to this dash alert but still contains useful info.
		For:             da.For,
		Updated:         time.Now().UTC(),
		Annotations:     annotations,
		Labels:          lbls,
		RuleGroupIndex:  1, // Every rule is in its own group.
		IsPaused:        isPaused,
		NoDataState:     transNoData(l, da.ParsedSettings.NoDataState),
		ExecErrState:    transExecErr(l, da.ParsedSettings.ExecutionErrorState),
	}

	// Label for routing and silences.
	n, v := getLabelForSilenceMatching(ar.UID)
	ar.Labels[n] = v

	if err := om.addErrorSilence(da, ar); err != nil {
		om.log.Error("Alert migration error: failed to create silence for Error", "rule_name", ar.Title, "err", err)
	}

	if err := om.addNoDataSilence(da, ar); err != nil {
		om.log.Error("Alert migration error: failed to create silence for NoData", "rule_name", ar.Title, "err", err)
	}

	return ar, nil
}

// migrateAlertRuleQueries attempts to fix alert rule queries so they can work in unified alerting. Queries of some data sources are not compatible with unified alerting.
func migrateAlertRuleQueries(l log.Logger, data []ngmodels.AlertQuery) ([]ngmodels.AlertQuery, error) {
	result := make([]ngmodels.AlertQuery, 0, len(data))
	for _, d := range data {
		// queries that are expression are not relevant, skip them.
		if d.DatasourceUID == expressionDatasourceUID {
			result = append(result, d)
			continue
		}
		var fixedData map[string]json.RawMessage
		err := json.Unmarshal(d.Model, &fixedData)
		if err != nil {
			return nil, err
		}
		// remove hidden tag from the query (if exists)
		delete(fixedData, "hide")
		fixedData = fixGraphiteReferencedSubQueries(fixedData)
		fixedData = fixPrometheusBothTypeQuery(l, fixedData)
		updatedModel, err := json.Marshal(fixedData)
		if err != nil {
			return nil, err
		}
		d.Model = updatedModel
		result = append(result, d)
	}
	return result, nil
}

// fixGraphiteReferencedSubQueries attempts to fix graphite referenced sub queries, given unified alerting does not support this.
// targetFull of Graphite data source contains the expanded version of field 'target', so let's copy that.
func fixGraphiteReferencedSubQueries(queryData map[string]json.RawMessage) map[string]json.RawMessage {
	fullQuery, ok := queryData[graphite.TargetFullModelField]
	if ok {
		delete(queryData, graphite.TargetFullModelField)
		queryData[graphite.TargetModelField] = fullQuery
	}

	return queryData
}

// fixPrometheusBothTypeQuery converts Prometheus 'Both' type queries to range queries.
func fixPrometheusBothTypeQuery(l log.Logger, queryData map[string]json.RawMessage) map[string]json.RawMessage {
	// There is the possibility to support this functionality by:
	//	- Splitting the query into two: one for instant and one for range.
	//  - Splitting the condition into two: one for each query, separated by OR.
	// However, relying on a 'Both' query instead of multiple conditions to do this in legacy is likely
	// to be unintentional. In addition, this would require more robust operator precedence in classic conditions.
	// Given these reasons, we opt to convert them to range queries and log a warning.

	var instant bool
	if instantRaw, ok := queryData["instant"]; ok {
		if err := json.Unmarshal(instantRaw, &instant); err != nil {
			// Nothing to do here, we can't parse the instant field.
			if isPrometheus, _ := isPrometheusQuery(queryData); isPrometheus {
				l.Info("Failed to parse instant field on Prometheus query", "instant", string(instantRaw), "err", err)
			}
			return queryData
		}
	}
	var rng bool
	if rangeRaw, ok := queryData["range"]; ok {
		if err := json.Unmarshal(rangeRaw, &rng); err != nil {
			// Nothing to do here, we can't parse the range field.
			if isPrometheus, _ := isPrometheusQuery(queryData); isPrometheus {
				l.Info("Failed to parse range field on Prometheus query", "range", string(rangeRaw), "err", err)
			}
			return queryData
		}
	}

	if !instant || !rng {
		// Only apply this fix to 'Both' type queries.
		return queryData
	}

	isPrometheus, err := isPrometheusQuery(queryData)
	if err != nil {
		l.Info("Unable to convert alert rule that resembles a Prometheus 'Both' type query to 'Range'", "err", err)
		return queryData
	}
	if !isPrometheus {
		// Only apply this fix to Prometheus.
		return queryData
	}

	// Convert 'Both' type queries to `Range` queries by disabling the `Instant` portion.
	l.Warn("Prometheus 'Both' type queries are not supported in unified alerting. Converting to range query.")
	queryData["instant"] = []byte("false")

	return queryData
}

// isPrometheusQuery checks if the query is for Prometheus.
func isPrometheusQuery(queryData map[string]json.RawMessage) (bool, error) {
	ds, ok := queryData["datasource"]
	if !ok {
		return false, fmt.Errorf("missing datasource field")
	}
	var datasource struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(ds, &datasource); err != nil {
		return false, fmt.Errorf("failed to parse datasource '%s': %w", string(ds), err)
	}
	if datasource.Type == "" {
		return false, fmt.Errorf("missing type field '%s'", string(ds))
	}
	return datasource.Type == "prometheus", nil
}

func ruleAdjustInterval(freq int64) int64 {
	// 10 corresponds to the SchedulerCfg, but TODO not worrying about fetching for now.
	var baseFreq int64 = 10
	if freq <= baseFreq {
		return 10
	}
	return freq - (freq % baseFreq)
}

func transNoData(l log.Logger, s string) ngmodels.NoDataState {
	switch legacymodels.NoDataOption(s) {
	case legacymodels.NoDataSetOK:
		return ngmodels.OK // values from ngalert/models/rule
	case "", legacymodels.NoDataSetNoData:
		return ngmodels.NoData
	case legacymodels.NoDataSetAlerting:
		return ngmodels.Alerting
	case legacymodels.NoDataKeepState:
		return ngmodels.NoData // "keep last state" translates to no data because we now emit a special alert when the state is "noData". The result is that the evaluation will not return firing and instead we'll raise the special alert.
	default:
		l.Warn("Unable to translate execution of NoData state. Using default execution", "old", s, "new", ngmodels.NoData)
		return ngmodels.NoData
	}
}

func transExecErr(l log.Logger, s string) ngmodels.ExecutionErrorState {
	switch legacymodels.ExecutionErrorOption(s) {
	case "", legacymodels.ExecutionErrorSetAlerting:
		return ngmodels.AlertingErrState
	case legacymodels.ExecutionErrorKeepState:
		// Keep last state is translated to error as we now emit a
		// DatasourceError alert when the state is error
		return ngmodels.ErrorErrState
	case legacymodels.ExecutionErrorSetOk:
		return ngmodels.OkErrState
	default:
		l.Warn("Unable to translate execution of Error state. Using default execution", "old", s, "new", ngmodels.ErrorErrState)
		return ngmodels.ErrorErrState
	}
}

// truncateRuleName truncates the rule name to the maximum allowed length.
func truncateRuleName(daName string) string {
	if len(daName) > store.AlertDefinitionMaxTitleLength {
		return daName[:store.AlertDefinitionMaxTitleLength]
	}
	return daName
}

// extractChannelIDs extracts the notification channel IDs from the given legacy dashboard alert parsed settings.
func extractChannelIDs(parsedSettings dashAlertSettings) (channelUids []uidOrID) {
	// Extracting channel UID/ID.
	for _, ui := range parsedSettings.Notifications {
		if ui.UID != "" {
			channelUids = append(channelUids, ui.UID)
			continue
		}
		// In certain circumstances, id is used instead of uid.
		// We add this if there was no uid.
		if ui.ID > 0 {
			channelUids = append(channelUids, ui.ID)
		}
	}

	return channelUids
}
