// Scenario 3: contention on one hot payment. setup() captures one large
// payment; every iteration refunds 1 cent of it with a fresh key. All of
// them need the same row lock (SELECT ... FOR UPDATE), so they run one at a
// time however much capacity the rest of the system has.
import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';
import { BASE, RUN, params, paymentBody, openModel, perEndpoint, summary } from './lib.js';

const endpoints = ['refund'];
export const options = {
  scenarios: { hot: openModel('hot') },
  thresholds: perEndpoint(endpoints),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const n = `${RUN}-hot`;
  const id = http.post(`${BASE}/v1/payments`, paymentBody(`fp_${n}`, 900000000000), params('setup', `c-${n}`)).json('id');
  http.post(`${BASE}/v1/payments/${id}/authorize`, null, params('setup', `a-${n}`));
  const r = http.post(`${BASE}/v1/payments/${id}/capture`, null, params('setup', `p-${n}`));
  if (r.status !== 200) throw new Error(`hot payment setup failed: ${r.status} ${r.body}`);
  return { id };
}

export function hot(data) {
  const r = http.post(`${BASE}/v1/payments/${data.id}/refunds`, JSON.stringify({ amount: 1 }),
    params('refund', `r-${RUN}-${exec.scenario.iterationInTest}`));
  check(r, { 'refund 201': (x) => x.status === 201 });
}

export const handleSummary = summary('hot', endpoints);
