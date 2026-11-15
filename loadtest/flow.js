// Scenario 1: the full flow. Each iteration creates a payment, authorizes it
// (through the simulated acquirer) and captures it: 3 requests, 3 database
// transactions plus the acquirer call. Every iteration uses fresh keys and
// its own card.
import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';
import { BASE, RUN, params, paymentBody, openModel, perEndpoint, summary } from './lib.js';

const endpoints = ['create', 'authorize', 'capture'];
export const options = {
  scenarios: { flow: openModel('flow') },
  thresholds: perEndpoint(endpoints),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function flow() {
  const n = `${RUN}-${exec.scenario.iterationInTest}`;
  let r = http.post(`${BASE}/v1/payments`, paymentBody(`fp_${n}`, 1000 + (exec.scenario.iterationInTest % 5000)), params('create', `c-${n}`));
  if (!check(r, { 'create 201': (x) => x.status === 201 })) return;
  const id = r.json('id');
  r = http.post(`${BASE}/v1/payments/${id}/authorize`, null, params('authorize', `a-${n}`));
  if (!check(r, { 'authorize 200/402': (x) => x.status === 200 || x.status === 402 }) || r.status !== 200) return;
  r = http.post(`${BASE}/v1/payments/${id}/capture`, null, params('capture', `p-${n}`));
  check(r, { 'capture 200': (x) => x.status === 200 });
}

export const handleSummary = summary('flow', endpoints);
