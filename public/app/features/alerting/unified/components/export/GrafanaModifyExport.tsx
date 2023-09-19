import { omit } from 'lodash';
import * as React from 'react';
import { useEffect, useState } from 'react';
import { useAsync } from 'react-use';

import { Alert, LoadingPlaceholder } from '@grafana/ui';

import { GrafanaRouteComponentProps } from '../../../../../core/navigation/types';
import { useDispatch } from '../../../../../types';
import { RuleIdentifier, RuleWithLocation } from '../../../../../types/unified-alerting';
import { RulerRuleDTO } from '../../../../../types/unified-alerting-dto';
import { fetchEditableRuleAction } from '../../state/actions';
import { RuleFormValues } from '../../types/rule-form';
import { rulerRuleToFormValues } from '../../utils/rule-form';
import * as ruleId from '../../utils/rule-id';
import { isGrafanaRulerRule } from '../../utils/rules';
import { AlertingPageWrapper } from '../AlertingPageWrapper';
import { ModifyExportRuleForm } from '../rule-editor/alert-rule-form/ModifyExportRuleForm';

interface GrafanaModifyExportProps extends GrafanaRouteComponentProps<{ id?: string }> {}

// TODO Duplicated in AlertRuleForm
const ignoreHiddenQueries = (ruleDefinition: RuleFormValues): RuleFormValues => {
  return {
    ...ruleDefinition,
    queries: ruleDefinition.queries?.map((query) => omit(query, 'model.hide')),
  };
};

function formValuesFromExistingRule(rule: RuleWithLocation<RulerRuleDTO>) {
  return ignoreHiddenQueries(rulerRuleToFormValues(rule));
}

export default function GrafanaModifyExport({ match }: GrafanaModifyExportProps) {
  const dispatch = useDispatch();

  const [ruleIdentifier, setRuleIdentifier] = useState<RuleIdentifier | undefined>(undefined);

  useEffect(() => {
    const identifier = ruleId.tryParse(match.params.id, true);
    setRuleIdentifier(identifier);
  }, [match.params.id]);

  const {
    loading,
    value: alertRule,
    error,
  } = useAsync(async () => {
    if (!ruleIdentifier) {
      return;
    }
    return await dispatch(fetchEditableRuleAction(ruleIdentifier)).unwrap();
  }, [ruleIdentifier]);

  if (!ruleIdentifier) {
    return <div>Rule not found</div>;
  }

  if (loading) {
    return <LoadingPlaceholder text="Loading the rule" />;
  }

  if (error) {
    return (
      <Alert title="Cannot load modify export" severity="error">
        {error.message}
      </Alert>
    );
  }

  return (
    <AlertingPageWrapper isLoading={loading} pageId="alert-list" pageNav={{ text: 'Modify export' }}>
      {alertRule && isGrafanaRulerRule(alertRule.rule) && (
        <ModifyExportRuleForm ruleForm={alertRule ? formValuesFromExistingRule(alertRule) : undefined} />
      )}
    </AlertingPageWrapper>
  );
}
