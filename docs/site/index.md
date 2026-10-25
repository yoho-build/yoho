---
layout: home
title: Yoho
titleTemplate: Deploy Docker Compose apps to your own servers
description: Put your app on your own server. Yoho deploys Docker Compose apps over SSH with one command and zero downtime. Free, MIT licensed.
markdownStyles: false
head:
  - - link
    - rel: preconnect
      href: https://fonts.googleapis.com
  - - link
    - rel: preconnect
      href: https://fonts.gstatic.com
      crossorigin: ''
  - - link
    - rel: stylesheet
      href: https://fonts.googleapis.com/css2?family=Newsreader:ital,opsz,wght@0,6..72,400;1,6..72,400&family=Geist:wght@400;600&family=Geist+Mono&display=swap
---

<script setup>
import { onMounted, ref } from 'vue'

// Reader switch: "plain" for people who run a product, "tech" for people who write the code.
const mode = ref('plain')
const copied = ref('Copy')
function setRead(m) {
  mode.value = m
  try { localStorage.setItem('yoho-read', m) } catch {}
}
onMounted(() => {
  try { const m = localStorage.getItem('yoho-read'); if (m === 'plain' || m === 'tech') mode.value = m } catch {}
})
async function copyInstall() {
  try {
    await navigator.clipboard.writeText('curl -fsSL https://yohobuild.com/install.sh | sh')
    copied.value = 'Copied'
  } catch {
    const r = document.createRange(); r.selectNodeContents(document.getElementById('install-cmd'))
    const s = getSelection(); s.removeAllRanges(); s.addRange(r)
    copied.value = 'Press ⌘C'
  }
  setTimeout(() => (copied.value = 'Copy'), 1800)
}
</script>

<div class="yh" :data-read="mode">
<main class="yh-wrap">
  <section class="yh-sec">
    <div class="yh-row">
      <span class="yh-label">Open source · Alpha</span>
      <div class="yh-switch" role="group" aria-label="Explain it for">
        <button type="button" :aria-pressed="String(mode === 'plain')" @click="setRead('plain')">I run a product</button>
        <button type="button" :aria-pressed="String(mode === 'tech')" @click="setRead('tech')">I write the code</button>
      </div>
    </div>
    <h1 id="yoho"><span class="plain">Put your app on <em>your own</em> server.</span><span class="tech">Deploy your Compose app <em>over SSH</em>.</span><a class="header-anchor" href="#yoho" aria-hidden="true"></a></h1>
    <p class="yh-muted plain">One command ships the app your team already built to a server you rent. It stays up while it updates, and there's no platform bill.</p>
    <p class="yh-muted tech">One command, zero downtime, and the server only needs Docker. Health-gated cutover through kamal-proxy, secrets resolved on your machine, backups built in.</p>
    <div class="yh-row">
      <a class="yh-btn primary" href="/quickstart/">5-minute quickstart</a>
      <a class="yh-btn" href="/agents/">Set up with your AI agent</a>
    </div>
    <p class="yh-proof">Free · MIT · alpha · your server · Docker is the only install</p>
    <figure class="yh-demo" aria-label="A deploy, and what visitors saw during it">
      <div class="yh-term">
<pre><span class="c">$</span> yoho deploy
<span class="t">▸</span> [primary] Build web, worker on the Server
<span class="t">▸</span> [primary] Start web, wait for health check
<span class="t">▸</span> [primary] Switch Proxy to the new web
<span class="ok">✓</span> [primary] Deploy 413591e38d08 <span class="c">15.9s</span>
Deployed shop@413591e38d08 to production
  <span class="t">→</span> https://shop.example.com   <span class="c"># sample</span></pre>
      </div>
      <div class="yh-cut" aria-hidden="true">
        <span class="yh-label">What visitors saw</span>
        <div class="yh-lane"><b>old</b><div class="yh-track"><span class="seg old" style="left:2%;width:70%"></span></div></div>
        <div class="yh-lane"><b>new</b><div class="yh-track"><span class="seg new" style="left:44%;width:54%"></span></div></div>
        <div class="yh-lane"><b>traffic</b><div class="yh-track"><span class="seg old" style="left:2%;width:62%"></span><span class="seg new" style="left:64%;width:34%"></span></div></div>
        <div class="yh-zero"><strong>0</strong><span class="yh-muted">requests dropped</span></div>
      </div>
    </figure>
  </section>
  <section class="yh-sec">
    <span class="yh-label">Where it runs</span>
    <h2 id="where-it-runs">Your laptop and your server. <em>Nothing</em> in between.<a class="header-anchor" href="#where-it-runs" aria-hidden="true"></a></h2>
    <p class="yh-muted plain">Yoho runs on your laptop and sends your app over a secure connection. Afterwards the server holds only your app and the doorway that routes visitors to it.</p>
    <p class="yh-muted tech">The CLI talks to the Server over SSH. The Server keeps Docker, your compose project and one <code>yoho-proxy</code> container: no control plane, no daemon, no database of ours.</p>
    <div class="yh-map-wrap">
      <svg class="yh-map" viewBox="0 0 600 250" role="img" aria-labelledby="map-title">
        <title id="map-title">Your laptop runs yoho and connects over SSH to your server, which runs Docker with the proxy and your app.</title>
        <rect class="box" x="16" y="20" width="190" height="210" rx="12"/>
        <text class="lbl" x="32" y="44">YOUR LAPTOP</text>
        <rect class="chip" x="32" y="58" width="158" height="32" rx="7"/><text class="mono" x="44" y="79">yoho</text>
        <rect class="chip" x="32" y="98" width="158" height="32" rx="7"/><text class="mono" x="44" y="119">compose.yaml</text>
        <rect class="chip" x="32" y="138" width="158" height="32" rx="7"/><text class="mono" x="44" y="159">secrets (1Password)</text>
        <text class="dim" x="32" y="204">You keep the keys.</text>
        <path class="wire" d="M212 125 H 322"/><path class="arrow" d="M322 119 L334 125 L322 131 Z"/>
        <text class="lbl" x="252" y="114">SSH</text>
        <rect class="box" x="340" y="20" width="244" height="210" rx="12"/>
        <text class="lbl" x="356" y="44">YOUR SERVER · DOCKER</text>
        <rect class="chip" x="356" y="58" width="212" height="32" rx="7"/><text x="368" y="79">yoho-proxy</text>
        <rect class="chip" x="356" y="98" width="100" height="32" rx="7"/><text x="368" y="119">web</text>
        <rect class="chip" x="468" y="98" width="100" height="32" rx="7"/><text x="480" y="119">worker</text>
        <rect class="chip" x="356" y="138" width="212" height="32" rx="7"/><text x="368" y="159">db + backups</text>
        <text class="dim" x="356" y="204">No agent. No platform.</text>
      </svg>
    </div>
  </section>
  <section class="yh-sec">
    <h2 id="three-commands" class="yh-label">Three commands<a class="header-anchor" href="#three-commands" aria-hidden="true"></a></h2>
    <div class="yh-steps">
      <div class="yh-step"><span class="n">01</span><code>yoho setup</code><h3>Prepare the server</h3><p>Installs Docker, a deploy user and a firewall on Debian or Ubuntu. <a href="/setup/">Server setup</a></p></div>
      <div class="yh-step"><span class="n">02</span><code>yoho deploy</code><h3>Ship it</h3><p>Builds, ships and switches traffic once the new version is healthy. <a href="/zero-downtime/">Zero downtime</a></p></div>
      <div class="yh-step"><span class="n">03</span><code>yoho ui</code><h3>Watch it</h3><p>A private dashboard on your laptop: health, load, releases, disk. <a href="/dashboards/">Dashboards</a></p></div>
    </div>
  </section>
  <section class="yh-sec">
    <h2 id="included" class="yh-label">Included<a class="header-anchor" href="#included" aria-hidden="true"></a></h2>
    <ul class="yh-list">
      <li><h3><a href="/configuration/">Keep your compose.yaml</a></h3><span><code>docker compose up</code> still works locally.</span></li>
      <li><h3><a href="/secrets/">Secrets stay out of the repo</a></h3><span>From 1Password, Bitwarden or any command.</span></li>
      <li><h3><a href="/backups/">Backups built in</a></h3><span>restic, S3, B2, SFTP or any rclone remote.</span></li>
      <li><h3><a href="/builds/">No registry needed</a></h3><span>Build on your Mac, ship over SSH.</span></li>
      <li><h3><a href="/cloudflare-tunnel/">HTTPS or Cloudflare Tunnel</a></h3><span>Custom domains, no open ports needed.</span></li>
      <li><h3><a href="/agents/">Agent-ready</a></h3><span>JSON output and a skill for coding agents.</span></li>
    </ul>
    <p class="yh-muted yh-small">Coming from another tool? <a href="/migrating-from-kamal/">Kamal</a>, <a href="/migrating-from-coolify/">Coolify</a>, <a href="/migrating-from-dokploy/">Dokploy</a>, <a href="/migrating-from-railway/">Railway</a>.</p>
  </section>
  <section class="yh-sec">
    <h2 id="questions" class="yh-label">Questions<a class="header-anchor" href="#questions" aria-hidden="true"></a></h2>
    <div>
      <details open><summary>Do I need to be an infrastructure engineer?</summary><p>No. If your app has a <code>compose.yaml</code>, the quickstart takes about five minutes, or your AI agent can do it with the bundled skill.</p></details>
      <details><summary>What does it cost?</summary><p>Yoho is free and MIT licensed. You pay only for your server.</p></details>
      <details><summary>Can I leave later?</summary><p>Yes. Your app stays a plain Compose project, and your data stays in normal Docker volumes on your server.</p></details>
    </div>
  </section>
  <section class="yh-sec">
    <h2 id="install">Free. Your server. <em>One command.</em><a class="header-anchor" href="#install" aria-hidden="true"></a></h2>
    <div class="yh-install">
      <code id="install-cmd">curl -fsSL https://yohobuild.com/install.sh | sh</code>
      <button type="button" @click="copyInstall">{{ copied }}</button>
    </div>
    <p class="yh-muted yh-small">Then <code>yoho init</code>, <code>yoho setup</code>, <code>yoho deploy</code>. <a href="/getting-started/">Getting started</a></p>
  </section>
</main>
</div>
