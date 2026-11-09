import http from 'node:http';
import { StringDecoder } from 'node:string_decoder';
import { appendFileSync } from 'node:fs';
import { createHash } from 'node:crypto';

// Every fault wraps a request to the real daemon, never a fake API server.
export class FaultProxy {
  constructor(upstream, artifactDir) {
    this.upstream = new URL(upstream);
    this.artifactDir = artifactDir;
    this.records = [];
    this.sockets = new Set();
    this.scenario = 'setup';
    this.rule = () => ({});
    this.server = http.createServer((req, res) => this.handle(req, res));
    this.server.on('connection', socket => {
      this.sockets.add(socket);
      socket.on('close', () => this.sockets.delete(socket));
    });
  }

  async listen() {
    await new Promise(resolve => this.server.listen(0, '127.0.0.1', resolve));
    this.domain = 'http://127.0.0.1:' + this.server.address().port;
    return this;
  }

  configure(scenario, rule = () => ({})) {
    this.scenario = scenario;
    this.rule = rule;
  }

  requests(scenario = this.scenario) {
    return this.records.filter(record => record.scenario === scenario);
  }

  async close() {
    for (const socket of this.sockets) socket.destroy();
    await new Promise(resolve => this.server.close(resolve));
  }

  handle(req, res) {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      let json;
      if (body.length && req.headers['content-type']?.includes('application/json')) {
        try { json = JSON.parse(body); } catch { /* Preserve malformed bytes. */ }
      }
      const record = {
        index: this.records.length + 1, scenario: this.scenario,
        method: req.method, path: req.url, startedAt: Date.now(),
        authenticated: Boolean(req.headers.authorization),
        lastEventId: req.headers['last-event-id'] ?? null,
        bodyBytes: body.length, bodySHA256: createHash('sha256').update(body).digest('hex'),
        ...(json ? { body: json } : {}),
      };
      this.records.push(record);
      const rule = this.rule(record) ?? {};
      record.fault = rule.name ?? null;
      let recorded = false;
      const save = () => {
        if (recorded) return;
        recorded = true;
        record.closedAt = Date.now();
        appendFileSync(this.artifactDir + '/requests.jsonl', JSON.stringify(record) + '\n');
      };
      res.on('close', save);
      const outgoing = http.request({
        hostname: this.upstream.hostname, port: this.upstream.port,
        path: req.url, method: req.method,
        headers: { ...req.headers, host: this.upstream.host, connection: 'close' },
      }, upstream => {
        record.upstreamStatus = upstream.statusCode;
        const headers = { ...upstream.headers };
        delete headers['content-length'];
        delete headers['transfer-encoding'];
        if (rule.statusCode) {
          upstream.resume();
          record.status = rule.statusCode;
          res.writeHead(rule.statusCode, { 'content-type': 'application/json', ...(rule.retryAfter ? { 'retry-after': rule.retryAfter } : {}) });
          res.end(JSON.stringify({ code: 'unavailable', message: 'E2E injected retryable response' }));
          return;
        }
        record.status = upstream.statusCode;
        if (headers['content-type']?.includes('text/event-stream') && rule.sse) {
          res.writeHead(upstream.statusCode, headers);
          res.flushHeaders();
          const decoder = new StringDecoder('utf8');
          let pending = '';
          let outputFrames = 0;
          let dropped = false;
          const write = raw => {
            if (!raw || dropped || res.destroyed) return;
            const wire = Buffer.from(rule.sse.crlf ? raw.replace(/\r?\n/g, '\r\n') : raw);
            if (rule.sse.fragmented) {
              // Split inside UTF-8 code points, not merely JS strings.
              for (let i = 0; i < wire.length; i += 7) res.write(wire.subarray(i, i + 7));
            } else res.write(wire);
          };
          if (rule.sse.prefix) write(rule.sse.prefix);
          const consume = () => {
            let delimiter;
            while (!dropped && (delimiter = /\r?\n\r?\n/.exec(pending))) {
              const raw = pending.slice(0, delimiter.index + delimiter[0].length);
              pending = pending.slice(delimiter.index + delimiter[0].length);
              const type = /^event:\s*(.+)$/m.exec(raw)?.[1]?.trim();
              const id = Number(/^id:\s*(\d+)$/m.exec(raw)?.[1] ?? 0);
              if ((type === 'stdout' || type === 'stderr') && rule.sse.partialOutputFrame === outputFrames + 1) {
                const dataStart = raw.indexOf('data: ') + 6;
                const dataEnd = raw.indexOf('\n', dataStart);
                const cutoff = dataStart + Math.floor((dataEnd - dataStart) / 2);
                write(raw.slice(0, cutoff));
                record.partialFrameId = id;
                record.partialFrameBytes = cutoff;
                dropped = true;
                // A graceful EOF mid-frame must retry from the delivered cursor.
                res.end();
                upstream.destroy();
                continue;
              }
              const transformed = rule.sse.transform ? rule.sse.transform(raw, { type, id }) : raw;
              write(transformed);
              rule.sse.onFrame?.(raw, { type, id });
              if (type === 'stdout' || type === 'stderr') outputFrames += 1;
              if (rule.sse.dropAfter && outputFrames >= rule.sse.dropAfter) {
                dropped = true;
                record.droppedAfterId = id;
                // End after a full frame: missing complete still requires retry.
                res.end();
                upstream.destroy();
              }
            }
          };
          upstream.on('data', chunk => { pending += decoder.write(chunk); consume(); });
          upstream.on('end', () => { pending += decoder.end(); consume(); if (!dropped) { write(pending); res.end(); } });
          upstream.on('error', () => { if (!res.destroyed) res.destroy(); });
          res.on('close', () => upstream.destroy());
          return;
        }
        if (rule.transformJSON || rule.disconnect || rule.hold || rule.onResponse || rule.truncate) {
          const responseChunks = [];
          upstream.on('data', chunk => responseChunks.push(chunk));
          upstream.on('end', () => {
            const responseBody = Buffer.concat(responseChunks);
            let value;
            try { value = JSON.parse(responseBody.toString('utf8')); } catch { /* binary */ }
            rule.onResponse?.(value, record);
            if (rule.disconnect) { res.destroy(); return; }
            if (rule.hold) return;
            if (rule.truncate) {
              res.writeHead(upstream.statusCode, { ...headers, 'content-length': responseBody.length });
              res.write(responseBody.subarray(0, Math.floor(responseBody.length / 2)));
              // Let headers and the partial body reach fetch before cutting TCP.
              setTimeout(() => res.destroy(), 30);
              return;
            }
            const outgoingBody = rule.transformJSON ? Buffer.from(JSON.stringify(rule.transformJSON(value))) : responseBody;
            res.writeHead(upstream.statusCode, headers);
            res.end(outgoingBody);
          });
          upstream.on('error', () => res.destroy());
          return;
        }
        res.writeHead(upstream.statusCode, headers);
        upstream.pipe(res);
        res.on('close', () => upstream.destroy());
      });
      outgoing.on('error', error => {
        record.upstreamError = error.message;
        if (!res.headersSent && !res.destroyed) res.writeHead(502).end();
        else res.destroy();
      });
      outgoing.end(body);
    });
  }
}
