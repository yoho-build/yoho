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
import { agentPrompt, installCommands } from './.vitepress/theme/prompt.js'
</script>

<div class="yh">
<main class="yh-wrap">
  <section class="yh-hero">
    <h1 id="yoho">Put your app on <em>your own</em> server.<a class="header-anchor" href="#yoho" aria-hidden="true"></a></h1>
    <p class="yh-lede">One command deploys your Docker Compose app over SSH, with zero downtime. Free and open source.</p>
  </section>
  <section class="yh-start">
    <div class="yh-way">
      <h2 id="ask-your-agent">Ask your AI agent<a class="header-anchor" href="#ask-your-agent" aria-hidden="true"></a></h2>
      <CopyBox :text="agentPrompt" label="Claude Code, Codex or Cursor, in your repo" />
      <p class="yh-note">It asks before every step. <a href="/agents/">What the agent does</a></p>
    </div>
    <div class="yh-way">
      <h2 id="do-it-yourself">Or do it yourself<a class="header-anchor" href="#do-it-yourself" aria-hidden="true"></a></h2>
      <CopyBox :text="installCommands" label="Terminal, in your repo" mono />
      <p class="yh-note">Any Debian or Ubuntu server with SSH. <a href="/quickstart/">5-minute quickstart</a></p>
    </div>
  </section>
  <section class="yh-shot">
<pre aria-label="Sample output of yoho deploy"><span class="c">$</span> yoho deploy
<span class="t">▸</span> Build web, worker
<span class="t">▸</span> Start web, wait for health check
<span class="t">▸</span> Switch traffic to the new web
<span class="ok">✓</span> Deployed shop to production <span class="c">15.9s</span></pre>
    <p class="yh-note">Traffic moves only when the new version is healthy, so visitors never see downtime.<br>Free · MIT · No platform, no agent on your server · <a href="/migrating-from-kamal/">Coming from Kamal</a>, <a href="/migrating-from-coolify/">Coolify</a> or <a href="/migrating-from-railway/">Railway</a>?</p>
  </section>
</main>
</div>
