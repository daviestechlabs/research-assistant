(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const dialog = $('indexing-dialog');
  let snapshot, timer, busy = false, reloading = false;
  const number = n => Number(n || 0).toLocaleString();
  const node = (tag, text, cls) => { const el = document.createElement(tag); if (text !== undefined) el.textContent = text; if (cls) el.className = cls; return el; };
  async function request(path, body) {
    const response = await fetch(path, body === undefined ? {} : {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
    if (!response.ok) { const text = await response.text(); throw new Error(response.status === 401 ? 'Sign in to manage your library.' : text.slice(0, 300)); }
    return response.json();
  }
  function error(e) { $('indexing-status').textContent = e.message; }
  async function refresh() {
    try {
      snapshot = await request('/api/library/indexing');
      const updated = Date.parse(snapshot.worker_heartbeat), live = Number.isFinite(updated) && Date.now() - updated < 45000;
      $('index-worker-status').textContent = live ? 'Offline worker is connected. Indexing makes no chat model calls.' : 'Worker is unavailable or has not checked in. Requests stay saved.';
      $('index-rollback').disabled = !snapshot.rollback_available || busy;
      $('index-active').textContent = snapshot.active === 'original' ? 'Original catalog' : 'Generation ' + snapshot.active.slice(0, 8);
      if (!$('index-collections').children.length) {
        for (const collection of snapshot.collections) {
          const label = node('label', undefined, 'index-collection'), checkbox = document.createElement('input');
          checkbox.type = 'checkbox'; checkbox.value = collection.id; checkbox.checked = collection.adapter !== 'inventory';
          label.append(checkbox, node('span', collection.label), node('small', collection.source + ' · ' + collection.adapter));
          $('index-collections').append(label);
        }
        $('index-meaning').disabled = !snapshot.meaning_available; $('index-meaning').checked = snapshot.meaning_available;
      }
      const runs = snapshot.runs || []; $('index-runs').replaceChildren();
      for (const run of runs) {
        const card = node('article', undefined, 'index-run');
        card.append(node('h3', run.status === 'ready' ? 'Validated candidate' : run.phase.replaceAll('-', ' ')), node('p', run.status + ' · ' + new Date(run.updated).toLocaleString(), 'index-meta'));
        const steps = ['inventory', 'keyword', 'meaning', 'validate', 'activate'];
        const activeStep = run.status === 'ready' ? 4 : run.phase === 'meaning' ? 2 : run.phase.startsWith('validate') ? 3 : run.phase === 'keyword' ? 1 : 0;
        const track = node('ol', undefined, 'index-steps'); steps.forEach((step, i) => track.append(node('li', step, i === activeStep ? 'current' : ''))); card.append(track);
        for (const inventory of run.inventory || []) {
          const label = snapshot.collections.find(c => c.id === inventory.collection)?.label || inventory.collection;
          if (snapshot.collections.find(c => c.id === inventory.collection)?.adapter === 'inventory') { card.append(node('p', label + ': ' + number(inventory.observed_objects) + ' saved objects observed · ' + number(inventory.metadata_candidates) + ' potential metadata objects · ' + (inventory.listing_complete ? 'listing finished' : 'listing in progress'))); continue; }
          card.append(node('p', label + ': ' + number(inventory.records) + ' metadata rows · ' + number(inventory.existing) + ' currently indexed · ' + number(inventory.object_count) + ' metadata objects'));
        }
        card.append(node('p', number(run.records_read) + ' rows processed · ' + number(run.duplicate_records) + ' identical duplicates · ' + number(run.missing_summaries) + ' missing summaries · ' + number(run.vectors) + ' vectors indexed · ' + number(run.reused_vectors) + ' reused · ' + number(run.new_embeddings) + ' new embeddings', 'index-meta'));
        if (run.validation) {
          card.append(node('p', number(run.validation.added_records) + ' records added · ' + number(run.validation.removed_records) + ' removed · ' + number(run.validation.changed_records) + ' changed'));
          card.append(node('p', 'Integrity, keyword search, and snapshot counts passed. ' + number(run.validation.saved_links_verified) + ' configured saved-link samples verified. ' + (run.validation.verified_vectors ? number(run.validation.verified_vectors) + ' vectors independently counted.' : 'This candidate uses keyword search.')));
          card.append(node('p', run.validation.saved_link_scope, 'index-meta'));
        }
        if (run.error) card.append(node('p', run.error, 'index-error'));
        const actions = node('div', undefined, 'index-actions');
        for (const action of run.status === 'ready' ? ['activate'] : ['queued', 'running'].includes(run.status) ? ['pause', 'cancel'] : ['paused', 'failed'].includes(run.status) ? ['resume', 'cancel'] : []) {
          const button = node('button', action === 'activate' ? 'Activate this catalog' : action[0].toUpperCase() + action.slice(1), 'quiet-button');
          button.disabled = busy || action === 'activate' && snapshot.active === run.id;
          button.addEventListener('click', () => change(run.id, action)); actions.append(button);
        }
        card.append(actions); $('index-runs').append(card);
      }
    } catch (e) { if (reloading) $('indexing-status').textContent = 'Reloading the catalog. This page will reconnect.'; else error(e); }
  }
  async function change(id, action) {
    busy = true;
    try {
      await request('/api/library/indexing/' + encodeURIComponent(id) + '/' + action, {});
      $('indexing-status').textContent = action === 'pause' ? 'Pause requested. The worker saves its current batch before stopping.' : 'Request saved.';
      if (['activate', 'rollback'].includes(action)) {
        reloading = true; $('indexing-status').textContent = 'Switch saved. The server will reload; your sessions stay on the NAS.';
        const poll = setInterval(async () => { try { const next = await request('/api/library/indexing'); if (action === 'rollback' || next.active === id) { clearInterval(poll); location.reload(); } } catch (_) {} }, 5000);
        return;
      }
    } catch (e) { error(e); } finally { busy = false; }
    await refresh();
  }
  async function submit(kind) {
    if (busy) return;
    const collections = [...$('index-collections').querySelectorAll('input:checked')].map(input => input.value);
    if (!collections.length) { error(new Error('Select at least one configured collection.')); return; }
    busy = true;
    try { await request('/api/library/indexing', {kind, collections, meaning: kind === 'rebuild' && $('index-meaning').checked}); $('indexing-status').textContent = 'Request saved. You can close this page and return later.'; }
    catch (e) { error(e); } finally { busy = false; }
    await refresh();
  }
  $('open-indexing').addEventListener('click', async () => { dialog.showModal(); await refresh(); timer = setInterval(refresh, 5000); });
  $('close-indexing').addEventListener('click', () => dialog.close()); dialog.addEventListener('close', () => clearInterval(timer));
  $('index-inventory').addEventListener('click', () => submit('inventory'));
  $('index-rebuild').addEventListener('click', () => submit('rebuild'));
  $('index-rollback').addEventListener('click', () => change('previous', 'rollback'));
  request('/api/stats').then(stats => { $('open-indexing').hidden = !stats.indexing_enabled; }).catch(() => {});
})();
