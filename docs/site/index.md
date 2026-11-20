---
layout: home
title: Yoho
titleTemplate: Deploy Docker Compose apps to your own servers
description: Deploy Docker Compose apps to your own servers over SSH with health-gated proxied cutovers, file-delivered secrets, and verified restore drills. Free, MIT licensed.
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
import { agentPrompt, installCommands } from './.vitepress/theme/prompt.js'
</script>

<div class="yh">
<main class="yh-wrap">
  <section class="yh-hero">
    <h1 id="yoho">Put your app on <em>your own</em> server.<a class="header-anchor" href="#yoho" aria-hidden="true"></a></h1>
    <p class="yh-lede">Deploy Docker Compose apps over SSH with one binary: health-gated zero-downtime cutovers for proxied Services, file-delivered secrets, verified restore drills, and a plain <code>docker compose</code> exit path. Free, MIT licensed (beta).</p>
  </section>
  <section class="yh-start">
    <div class="yh-way">
      <h2 id="ask-your-agent">Ask your AI agent<a class="header-anchor" href="#ask-your-agent" aria-hidden="true"></a></h2>
      <CopyBox :text="agentPrompt" label="Claude Code, Codex or Cursor, in your repo" />
      <p class="yh-note">Review <code>yoho plan</code> before running <code>yoho apply</code>. <a href="/agents/">What the agent does</a> · <a href="/team-access/">Scoped Gate access</a></p>
    </div>
    <div class="yh-way">
      <h2 id="do-it-yourself">Or do it yourself<a class="header-anchor" href="#do-it-yourself" aria-hidden="true"></a></h2>
      <CopyBox :text="installCommands" label="Terminal, in your repo" mono />
      <p class="yh-note">Walk through a full deploy, rollback, and secret change in the <a href="/quickstart/">5-minute quickstart</a>.</p>
    </div>
  </section>
  <section class="yh-shot">
<pre aria-label="Sample output of yoho deploy and yoho backup drill"><span class="c">$</span> yoho deploy
<span class="t">▸</span> Build web, worker
<span class="t">▸</span> Start web, wait for health check
<span class="t">▸</span> Switch traffic to the new web
<span class="ok">✓</span> Deployed shop to production <span class="c">15.9s</span>
<span class="c">$</span> yoho backup drill
<span class="ok">✓</span> Restored latest backup into throwaway project, verified, RTO estimate <span class="c">4.7s</span></pre>
    <p class="yh-note">Proxied Services switch without downtime when healthy; workers and Stateful Services restart stop-first, and upgrading the shared Proxy causes a brief (~1 s) interruption. Stateful volumes are pinned, never <code>down -v</code>, and recoverable via <a href="/backups/#losing-a-server">Losing a Server</a>.<br>Free · MIT · No platform or daemon on your server · <a href="/support-matrix/">Support matrix</a> · <a href="/compare/">Compare</a> · <a href="/deploy-path/">Deploy path</a> · <a href="/migrating-from-kamal/">Coming from Kamal</a>, <a href="/migrating-from-coolify/">Coolify</a> or <a href="/migrating-from-railway/">Railway</a>?</p>
  </section>
</main>
</div>
