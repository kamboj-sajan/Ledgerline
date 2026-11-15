// Shared helpers for the Ledgerline k6 scenarios.
import http from 'k6/http';

export const BASE = __ENV.BASE_URL || 'http://localhost:8080';
export const API_KEY = __ENV.API_KEY;
export const RUN = __ENV.RUN_ID || String(Date.now());
export const RATE = Number(__ENV.RATE || 50);         // iterations per second
export const DURATION = __ENV.DURATION || '30s';

// Declines (402) are a normal business outcome, not a failed request.
http.setResponseCallback(http.expectedStatuses(200, 201, 402));

export function params(name, idemKey) {
  const headers = { Authorization: `Bearer ${API_KEY}`, 'Content-Type': 'application/json' };
  if (idemKey) headers['Idempotency-Key'] = idemKey;
  return { headers, tags: { name } };
}

export function paymentBody(card, amount) {
  return JSON.stringify({
    amount, currency: 'USD', customer_id: 'cus_load',
    card: { fingerprint: card, country: 'US' }, ip_country: 'US',
  });
}

// openModel is a constant-arrival-rate scenario: k6 starts RATE iterations
// per second whether or not earlier ones finished, so a slow server shows up
// as latency and dropped iterations instead of silently lowering the load.
export function openModel(exec) {
  return {
    executor: 'constant-arrival-rate', exec, rate: RATE, timeUnit: '1s', duration: DURATION,
    preAllocatedVUs: Math.min(2000, Math.max(20, RATE * 2)), maxVUs: 4000,
  };
}

// Sub-metrics per endpoint only exist if a threshold names them. These
// thresholds never fail; they make k6 compute latency and error rate per
// endpoint, which keeps setup() traffic out of the reported numbers.
export function perEndpoint(names) {
  const t = {};
  for (const n of names) {
    t[`http_req_duration{name:${n}}`] = ['p(99)>=0'];
    t[`http_req_failed{name:${n}}`] = ['rate<=1'];
    t[`http_reqs{name:${n}}`] = ['count>=0'];
  }
  return t;
}

function ms(v) { return v === undefined ? '-' : v.toFixed(1); }
function seconds(d) { const m = /^(\d+)s$/.exec(d); return m ? Number(m[1]) : NaN; }

// summary writes the raw k6 summary as JSON and markdown table rows (one
// per endpoint), so every number in docs/benchmarks.md traces back to a
// k6 run. The achieved rate is completed iterations divided by the
// scenario's duration (k6's own iterations.rate also counts setup time).
export function summary(scenario, endpoints) {
  return (data) => {
    const m = data.metrics;
    const iterCount = m.iterations ? m.iterations.values.count : 0;
    const dropped = m.dropped_iterations ? m.dropped_iterations.values.count : 0;
    const secs = seconds(DURATION);
    let rows = '';
    for (const n of endpoints) {
      const d = m[`http_req_duration{name:${n}}`];
      const f = m[`http_req_failed{name:${n}}`];
      const c = m[`http_reqs{name:${n}}`];
      if (!d) continue;
      const v = d.values;
      rows += `| ${scenario} | ${RATE} | ${(iterCount / secs).toFixed(1)} | ${n} | ${(c.values.count / secs).toFixed(1)} | ${ms(v.med)} | ${ms(v['p(95)'])} | ${ms(v['p(99)'])} | ${ms(v.max)} | ${(100 * f.values.rate).toFixed(2)}% | ${dropped} |\n`;
    }
    const base = `${__ENV.OUT_DIR || 'loadtest/results'}/${scenario}-${RATE}`;
    return { [`${base}.json`]: JSON.stringify(data, null, 2), [`${base}.row.md`]: rows, stdout: rows };
  };
}
