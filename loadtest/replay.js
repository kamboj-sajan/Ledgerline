// Scenario 2: idempotent replays. setup() captures a pool of payments; every
// iteration then re-sends one of those captures with its original key. Each
// request is a key lookup and a replay of the stored bytes: no state change,
// no ledger write.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, RUN, params, paymentBody, openModel, perEndpoint, summary } from './lib.js';

const endpoints = ['replay'];
export const options = {
  scenarios: { replay: openModel('replay') },
  thresholds: perEndpoint(endpoints),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  setupTimeout: '120s',
};

export function setup() {
  const pool = [];
  for (let i = 0; i < 200; i++) {
    const n = `${RUN}-seed-${i}`;
    const id = http.post(`${BASE}/v1/payments`, paymentBody(`fp_${n}`, 2000), params('setup', `c-${n}`)).json('id');
    http.post(`${BASE}/v1/payments/${id}/authorize`, null, params('setup', `a-${n}`));
    const key = `p-${n}`;
    const r = http.post(`${BASE}/v1/payments/${id}/capture`, null, params('setup', key));
    if (r.status === 200) pool.push({ id, key });
  }
  return { pool };
}

export function replay(data) {
  const p = data.pool[Math.floor(Math.random() * data.pool.length)];
  const r = http.post(`${BASE}/v1/payments/${p.id}/capture`, null, params('replay', p.key));
  check(r, { 'replayed 200': (x) => x.status === 200 && x.headers['Idempotent-Replayed'] === 'true' });
}

export const handleSummary = summary('replay', endpoints);
