#!/usr/bin/env node
'use strict';

// Transporte WhatsApp/Baileys para o PrimeCel Gestor.
// A regra de negócio fica no binário Go: primecel-gestor whatsapp-handle.

const { spawnSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const pino = require('pino');
const qrcode = require('qrcode-terminal');

const BIN = process.env.PRIMECEL_BIN || process.env.PRIMECEL_GESTOR_BIN || '/opt/primecel-gestor/primecel-gestor';
const ENV_FILE = process.env.PRIMECEL_ENV_FILE || process.env.CONFIG_ENV || '/etc/primecel-gestor/config.env';
const logger = pino({ level: process.env.WHATSAPP_LOG_LEVEL || 'silent' });

function parseBoolEnv(name, def) {
  const v = String(process.env[name] || '').trim().toLowerCase();
  if (!v) return def;
  return ['1', 'true', 'sim', 'yes', 'on'].includes(v);
}

function writePairStatus(status) {
  const statusFile = process.env.WHATSAPP_STATUS_FILE || '';
  if (!statusFile) return;
  try { fs.writeFileSync(statusFile, `${status}\n`, { mode: 0o600 }); } catch (_) {}
}

function printQRCode(qr) {
  const compact = parseBoolEnv('WHATSAPP_QR_COMPACT', true);
  const stdout = parseBoolEnv('WHATSAPP_QR_STDOUT', true);
  const outFile = process.env.WHATSAPP_QR_FILE || '/tmp/primecel-whatsapp-qr.txt';
  qrcode.generate(qr, { small: compact }, (out) => {
    try { fs.writeFileSync(outFile, `${out}\n`, { mode: 0o600 }); } catch (_) {}
    writePairStatus('qr');
    if (stdout) {
      console.log('');
      console.log('================ PRIMECEL WHATSAPP QR CODE ================');
      console.log('Escaneie pelo WhatsApp > Aparelhos conectados.');
      console.log(out);
      console.log(`QR completo salvo em: ${outFile}`);
      console.log('============================================================');
      console.log('');
    }
  });
}

function callBackend(from, text, extra = {}) {
  const payload = JSON.stringify({ from, text, ...extra });
  const args = ['whatsapp-handle', '--json', payload];
  const env = { ...process.env, CONFIG_ENV: ENV_FILE, PRIMECEL_ENV_FILE: ENV_FILE };
  const res = spawnSync(BIN, args, { encoding: 'utf8', env, timeout: 45000 });
  if (res.error) throw res.error;
  if (res.status !== 0) {
    throw new Error((res.stderr || res.stdout || `backend saiu com código ${res.status}`).trim());
  }
  return JSON.parse(res.stdout || '{}');
}

function messageText(msg) {
  const m = msg.message || {};
  if (m.conversation) return m.conversation;
  if (m.extendedTextMessage && m.extendedTextMessage.text) return m.extendedTextMessage.text;
  if (m.imageMessage && m.imageMessage.caption) return m.imageMessage.caption;
  if (m.videoMessage && m.videoMessage.caption) return m.videoMessage.caption;
  if (m.buttonsResponseMessage && m.buttonsResponseMessage.selectedButtonId) return m.buttonsResponseMessage.selectedButtonId;
  if (m.listResponseMessage && m.listResponseMessage.singleSelectReply) return m.listResponseMessage.singleSelectReply.selectedRowId || '';
  if (m.templateButtonReplyMessage && m.templateButtonReplyMessage.selectedId) return m.templateButtonReplyMessage.selectedId;
  return '';
}

function jidFor(to, fallbackJid) {
  const digits = String(to || '').replace(/\D+/g, '');
  return digits ? `${digits}@s.whatsapp.net` : fallbackJid;
}

async function sendResponse(sock, jid, response, quoted) {
  const messages = Array.isArray(response.messages) ? response.messages : [];
  for (const msg of messages) {
    if (typeof msg === 'string') {
      await sock.sendMessage(jid, { text: msg }, quoted ? { quoted } : undefined);
      continue;
    }
    const targetJid = jidFor(msg && msg.to, jid);
    if (msg && msg.text) {
      await sock.sendMessage(targetJid, { text: msg.text }, quoted && targetJid === jid ? { quoted } : undefined);
      continue;
    }
    if (msg && msg.image) {
      await sock.sendMessage(targetJid, {
        image: { url: msg.image },
        caption: msg.caption || ''
      }, quoted && targetJid === jid ? { quoted } : undefined);
      continue;
    }
    if (msg && msg.document) {
      await sock.sendMessage(targetJid, {
        document: { url: msg.document },
        fileName: msg.fileName || path.basename(msg.document),
        mimetype: msg.mimetype || 'application/octet-stream',
        caption: msg.caption || ''
      }, quoted && targetJid === jid ? { quoted } : undefined);
      continue;
    }
    await sock.sendMessage(targetJid, { text: JSON.stringify(msg) }, quoted && targetJid === jid ? { quoted } : undefined);
  }
}

async function startBaileys(options = {}) {
  let baileys;
  try {
    baileys = require('@whiskeysockets/baileys');
  } catch (err) {
    console.error('Baileys não instalado. Execute npm install dentro da pasta whatsapp/.');
    process.exit(1);
  }
  const {
    default: makeWASocket,
    useMultiFileAuthState,
    DisconnectReason,
    downloadContentFromMessage,
    fetchLatestBaileysVersion
  } = baileys;

  const authDir = process.env.WHATSAPP_AUTH_DIR || '/etc/primecel-gestor/whatsapp-auth';
  fs.mkdirSync(authDir, { recursive: true });
  const { state, saveCreds } = await useMultiFileAuthState(authDir);

  let version;
  try {
    const latest = await fetchLatestBaileysVersion();
    version = latest && latest.version;
  } catch (_) {
    version = undefined;
  }

  const pairOnce = !!options.pairOnce;
  const pairTimeoutMs = Number(process.env.WHATSAPP_PAIR_TIMEOUT_MS || '180000');
  let pairTimer = null;
  if (pairOnce) {
    pairTimer = setTimeout(() => {
      writePairStatus('timeout');
      if (!pairOnce || parseBoolEnv('WHATSAPP_PAIR_VERBOSE', false)) console.log('Tempo de sincronização do WhatsApp finalizado. Se não conectou, execute novamente a opção do instalador.');
      process.exit(0);
    }, pairTimeoutMs);
  }

  const sock = makeWASocket({
    version,
    auth: state,
    printQRInTerminal: false,
    logger,
    browser: ['Primecel Bot', 'Chrome', '1.0.0'],
    markOnlineOnConnect: false
  });

  sock.ev.on('creds.update', saveCreds);
  sock.ev.on('connection.update', ({ connection, lastDisconnect, qr }) => {
    if (qr) printQRCode(qr);
    if (connection === 'open') {
      writePairStatus('connected');
      if (!pairOnce || parseBoolEnv('WHATSAPP_PAIR_VERBOSE', false)) console.log('PrimeCel WhatsApp conectado.');
      if (pairTimer) clearTimeout(pairTimer);
      if (pairOnce) {
        setTimeout(() => process.exit(0), 800);
      }
    }
    if (connection === 'close') {
      const code = lastDisconnect && lastDisconnect.error && lastDisconnect.error.output ? lastDisconnect.error.output.statusCode : 0;
      const shouldReconnect = code !== DisconnectReason.loggedOut;
      if (!pairOnce || parseBoolEnv('WHATSAPP_PAIR_VERBOSE', false)) {
        console.log(`Conexão WhatsApp fechada. Reconnect=${shouldReconnect} code=${code}`);
      }
      if (shouldReconnect) {
        if (pairOnce) writePairStatus('reconnecting');
        setTimeout(() => startBaileys(options).catch((err) => {
          if (pairOnce) writePairStatus('error');
          if (!pairOnce || parseBoolEnv('WHATSAPP_PAIR_VERBOSE', false)) console.error(err);
        }), 3000);
      } else {
        writePairStatus('logged_out');
        if (!pairOnce || parseBoolEnv('WHATSAPP_PAIR_VERBOSE', false)) console.log('Sessão desconectada. Gere um novo QR Code e conecte novamente.');
        if (pairOnce) setTimeout(() => process.exit(0), 500);
      }
    }
  });

  sock.ev.on('messages.upsert', async ({ messages }) => {
    for (const m of messages || []) {
      if (!m.message || m.key.fromMe) continue;
      const jid = m.key.remoteJid;
      if (!String(jid || '').endsWith('@s.whatsapp.net')) continue;
      const from = String(jid || '').replace(/@.*/, '').replace(/\D+/g, '');
      const imageMessage = m.message.imageMessage;
      const text = messageText(m).trim();
      let mediaPath = '';
      let mediaType = '';
      if (imageMessage) {
        const tmpDir = process.env.WHATSAPP_MEDIA_DIR || '/tmp/primecel-wa-media';
        fs.mkdirSync(tmpDir, { recursive: true });
        const fileName = `wa-${Date.now()}-${Math.random().toString(16).slice(2)}.jpg`;
        const outPath = path.join(tmpDir, fileName);
        const stream = await downloadContentFromMessage(imageMessage, 'image');
        const chunks = [];
        for await (const chunk of stream) chunks.push(chunk);
        fs.writeFileSync(outPath, Buffer.concat(chunks));
        mediaPath = outPath;
        mediaType = 'image';
      }
      if (!text && !mediaPath) continue;
      try {
        const response = callBackend(from, text, { media_path: mediaPath, media_type: mediaType });
        await sendResponse(sock, jid, response, m);
      } catch (err) {
        await sock.sendMessage(jid, { text: `❌ Erro interno: ${err.message}` }, { quoted: m });
      }
    }
  });
}

if (require.main === module) {
  if (process.argv[2] === '--test-backend') {
    const from = process.argv[3] || '5500000000000';
    const text = process.argv.slice(4).join(' ') || 'menu';
    console.log(JSON.stringify(callBackend(from, text), null, 2));
  } else if (process.argv[2] === '--pair-once') {
    startBaileys({ pairOnce: true }).catch((err) => { console.error(err); process.exit(1); });
  } else {
    startBaileys().catch((err) => { console.error(err); process.exit(1); });
  }
}

module.exports = { callBackend };
