// secrets.js — read a Secret value through the pod's own Kubernetes RBAC (the
// Registry serves only coordinates, never values — see docs/runtime-protocol.md
// §1.3). In-cluster only: reads the ServiceAccount token/CA/namespace files
// every pod gets, and talks straight to the API server over fetch — no
// @kubernetes/client-node dependency for what is one GET request.
'use strict';

import { readFileSync, existsSync } from 'node:fs';
import https from 'node:https';

const TOKEN_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/token';
const CA_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/ca.crt';

export class SecretReader {
  constructor(namespace) {
    this.namespace = namespace;
    this.host = process.env.KUBERNETES_SERVICE_HOST;
    this.port = process.env.KUBERNETES_SERVICE_PORT || '443';
    this.inCluster = Boolean(this.host) && existsSync(TOKEN_PATH);
    if (this.inCluster) {
      this.token = readFileSync(TOKEN_PATH, 'utf8').trim();
      this.ca = readFileSync(CA_PATH);
    }
  }

  // read returns the value of `key` in Secret `secretName`, or throws.
  async read(secretName, key) {
    if (!secretName) throw new Error('no secret coordinates: nothing configured to read');
    if (!this.inCluster) {
      throw new Error('not running in-cluster (no ServiceAccount token found); cannot read Secrets');
    }
    const path = `/api/v1/namespaces/${encodeURIComponent(this.namespace)}/secrets/${encodeURIComponent(secretName)}`;
    const body = await this._get(path);
    const secret = JSON.parse(body);
    const b64 = secret.data?.[key];
    if (b64 == null) throw new Error(`secret ${secretName} has no key ${key}`);
    return Buffer.from(b64, 'base64').toString('utf8');
  }

  _get(path) {
    return new Promise((resolve, reject) => {
      const req = https.request(
        { host: this.host, port: this.port, path, method: 'GET', ca: this.ca, headers: { Authorization: `Bearer ${this.token}` } },
        (res) => {
          let data = '';
          res.on('data', (d) => { data += d; });
          res.on('end', () => {
            if (res.statusCode !== 200) { reject(new Error(`k8s API ${res.statusCode}: ${data}`)); return; }
            resolve(data);
          });
        },
      );
      req.on('error', reject);
      req.end();
    });
  }
}
