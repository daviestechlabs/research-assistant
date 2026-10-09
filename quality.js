'use strict';
// Opening measured results makes no model request. Source text uses textContent.
function renderQuality(report){
 const box=el('quality-results');box.replaceChildren();
 renderGenerationAudit(box,report.generation);
 renderPairedRetrieval(box,report.paired_retrieval);
 if(report.status!=='complete'){
  const messages={missing:'No measured benchmark is available yet.',running:'The operator benchmark is running or was interrupted. Completed results are still pending.',stale:'The saved benchmark uses an older source, model configuration, or review policy. A fresh measurement is required.'};
  box.append(node('p',messages[report.status]||'The benchmark is unavailable.'));renderHoldoutQuality(box,report.holdout);renderGeneratedQuality(box,report.generated);return;
 }
 const c=report.counts;
 box.append(node('p','Measured '+new Date(report.completed_at).toLocaleString()+' · configured model '+report.configured_model));
 box.append(node('p','Comparison evidence reviewer: '+c.true_accepts+'/'+c.supported_cases+' supported claims kept; '+c.true_abstentions+'/'+c.unsupported_cases+' unsupported claims rejected.'));
 box.append(node('p','False acceptances: '+c.false_accepts+' · supported claims rejected: '+c.false_abstentions+'.'));
 box.append(node('p',report.annotation));
 const evidence=node('details','');evidence.append(node('summary','Inspect all '+report.outcomes.length+' claim decisions'));
 for(const [index,out] of report.outcomes.entries()){
  const test=report.cases[index],card=node('details','');
  const correct=out.accepted===out.expected_supported;
  card.append(node('summary',(correct?'Matches expectation: ':'Mismatch: ')+test.id));
  card.append(node('p','Candidate ('+test.category+'): '+out.finding.text));
  card.append(node('p','Expected '+(test.expected_supported?'supported':'insufficient')+'; reviewer '+(out.accepted?'supported':'insufficient')+' ('+out.reason+').'));
  card.append(node('p','Annotation: '+test.rationale));
  for(const citation of out.finding.citations){
   card.append(node('blockquote',citation.quote));
   if(citation.scope==='abstract'){
    card.append(sourceLink({id:citation.id,source:'arxiv',pdf_url:'/api/pdf?id='+encodeURIComponent(citation.id)},citation.id+' · abstract evidence · open saved PDF'));
   }else card.append(node('small','Synthetic adversarial text, not a real paper excerpt.'));
  }
  evidence.append(card);
 }
 box.append(evidence);
 renderHoldoutQuality(box,report.holdout);
 renderGeneratedQuality(box,report.generated);
 box.append(node('h3','Keyword retrieval baseline'));
 box.append(node('p',report.keyword_hits_at_10+'/'+report.keyword_retrieval.length+' curated target papers found in the first ten results. Mean reciprocal rank: '+report.keyword_mrr_at_10.toFixed(3)+'.'));
 const retrieval=node('details','');retrieval.append(node('summary','Inspect the eight queries'));
 for(const q of report.keyword_retrieval){retrieval.append(node('p',q.kind+': “'+q.query+'” → '+q.target_id+' · '+(q.rank_at_10?'rank '+q.rank_at_10:'not in first ten')))}
 box.append(retrieval,node('p',report.semantic_status));
 box.append(node('h3','Exact quotation validator'));
 const quotes=report.quote_validator;
 box.append(node('p',quotes.verified_excerpts+' authentic excerpt checks; '+quotes.fabricated_rejected+' fabricated excerpts and '+quotes.misbound_rejected+' excerpts attached to the wrong paper rejected.'));
 box.append(node('p',quotes.scope));
 box.append(node('h3','What this measurement covers'));
 for(const limit of report.limits)box.append(node('p',limit));
 box.append(node('p','Benchmark cost: '+report.model_calls+' local model calls and '+report.reserved_units+' reserved input/output units. Opening this report makes no model call.'));
}
function renderPairedRetrieval(box,report){
 box.append(node('h3','Meaning search versus keyword search'));
 const status=report?.status||'missing';
 if(status!=='complete'){
  const messages={missing:'No paired full-corpus measurement is available. It requires a complete, independently verified vector index.',stale:'The saved comparison uses an older source or index contract. A new measurement is required.',running:'This measurement is running or was interrupted. Its completed query pairs do not form a complete benchmark.',failed:'This measurement stopped before finishing. Its completed query pairs do not form a complete benchmark.'};
  box.append(node('p',messages[status]||'The paired measurement is unavailable.'));
  if(!report||status==='missing'||status==='stale')return;
 }else{
  box.append(node('p','Both methods searched all '+report.coverage.total.toLocaleString()+' supplied abstracts with the same eight queries and ten-result limit.'));
  box.append(node('p','Measured '+new Date(report.completed_at).toLocaleString()+'. These are reused development queries, not an estimate of overall relevance.'));
  for(const group of report.groups){
   box.append(node('p',(group.kind==='synonym'?'Paraphrase queries':'Keyword queries')+': keyword search found '+group.keyword_hits_at_10+'/'+group.queries+' curated targets; meaning search found '+group.meaning_hits_at_10+'/'+group.queries+'. Mean reciprocal rank: '+group.keyword_mrr_at_10.toFixed(3)+' versus '+group.meaning_mrr_at_10.toFixed(3)+'.'));
  }
  box.append(node('p','Reciprocal rank is one divided by the target rank, or zero when the target falls outside the first ten results.'));
 }
 const pairs=node('details','');pairs.append(node('summary','Inspect '+report.cases.length+' recorded query pairs'));
 for(const row of report.cases){
  const card=node('details','');card.append(node('summary',row.kind+': “'+row.query+'”'));
  card.append(sourceLink({id:row.target_id,source:'arxiv',pdf_url:'/api/pdf?id='+encodeURIComponent(row.target_id)},'Curated target · '+row.target_id+' · open saved PDF'));
  for(const [label,rank,hits] of [['Keyword',row.keyword_rank_at_10,row.keyword_results],['Meaning',row.meaning_rank_at_10,row.meaning_results]]){
   card.append(node('p',label+' target '+(rank?'rank '+rank:'not in first ten')+'.'));
   for(const hit of hits)card.append(sourceLink({id:hit.id,source:'arxiv',pdf_url:'/api/pdf?id='+encodeURIComponent(hit.id)},label+' result · '+hit.id+' · open saved PDF'));
  }
  pairs.append(card);
 }
 box.append(pairs,node('p','Recorded embedding reservations: '+report.embedding_calls+'; reserved input units: '+report.reserved_units+'. No chat-model call is part of this measurement.'));
 for(const limit of report.limits)box.append(node('p',limit));
}
function renderGenerationAudit(box,report){
 box.append(node('h3','Answer and graph generation checks'));
 if(report?.contract==='research-generation-audit-collection-v1'){
  box.append(node('p',report.audits.length+' separately recorded development runs. Each run retains its own failed requests and unrun cases.'));
  for(const limit of report.limits)box.append(node('p',limit));
  for(const [index,audit] of report.audits.entries()){
   const run=node('section','');run.append(node('h4','Recorded run '+(index+1)));
   renderGenerationAudit(run,audit);box.append(run);
  }
  return;
 }
 if(!report||!['measured-development-audit','partial-development-audit'].includes(report.status)){
  box.append(node('p',report?.status==='stale'?'This audit uses an older source or model contract. It remains historical evidence.':'No approved generation measurement is available.'));return;
 }
 if(report.status==='partial-development-audit')box.append(node('p','This run stopped before completing its planned checks. Failed or unrun requests provide no scientific result.'));
 if(report.selected_case)box.append(node('p','Selected-case run: '+report.selected_case+'. It does not measure the other development cases.'));
 if(report.unmeasured_cases?.length)box.append(node('p','Unrun cases: '+report.unmeasured_cases.join(', ')+'.'));
 const c=report.counts;
 box.append(node('p',c.measured_cases+'/'+c.planned_cases+' development cases measured · '+report.generation_dispatched_requests+' chat requests · '+c.manual_passes+' meaning checks passed; '+c.manual_failures+' failed; '+c.manual_unassessable+' unassessable.'));
 box.append(node('p','These are source-backed development annotations by '+report.annotator+'. They do not estimate overall accuracy.'));
 box.append(node('p','The measured run accepted '+c.accepted_outputs+' outputs. Acceptance means the response passed structure checks; it does not mean its scientific claims passed.'));
 const details=node('details','');details.append(node('summary','Inspect all '+report.cases.length+' generated cases'));
 for(const test of report.cases){
  const card=node('details','');card.append(node('summary',test.case_id+' · '+(test.synthetic_source?'inserted source instruction':'unchanged abstract')));
  if(test.generation_error)card.append(node('p','Generation stopped: '+test.generation_error));
  card.append(node('p','Reported relations retained: '+test.retained_reported_relations+'. Literal attack marker present: '+test.synthetic_marker_in_response+'.'));
  for(const [artifact,counts] of Object.entries(test.quote_provenance)){
   card.append(node('p',artifact+' distinct quotation provenance: '+Object.entries(counts.unique_quotes||{}).map(([kind,count])=>kind+' '+count).join(', ')));
  }
  for(const check of test.manual_checks){
   card.append(node('h4',check.verdict+': '+check.expectation),node('p',check.rationale));
   if(check.observation){const observed=node('pre',typeof check.observation.value==='string'?check.observation.value:JSON.stringify(check.observation.value,null,2));observed.className='full-summary';card.append(observed)}
   for(const basis of check.basis){
    card.append(node('blockquote',basis.quote));
    card.append(sourceLink({id:basis.id,source:'arxiv',pdf_url:'/api/pdf?id='+encodeURIComponent(basis.id)},basis.id+' · unchanged abstract basis · open saved PDF'));
   }
  }
  details.append(card);
 }
 box.append(details,node('p','Audited '+new Date(report.audited_at).toLocaleString()+' · configured model '+report.configured_model+'. Reading this report makes no model request.'));
 for(const limit of report.limits)box.append(node('p',limit));
}
function renderHoldoutQuality(box,report){
 box.append(node('h3','Held-out evidence review'));
 if(!report||report.status!=='complete'){
  const messages={missing:'The separate held-out review has not been measured.',running:'The held-out review is running or was interrupted.',stale:'The held-out receipt uses older sources, model configuration, or policy. Its cases cannot be reused as fresh holdouts.'};
  box.append(node('p',messages[report?.status]||'The held-out receipt is unavailable.'));return;
 }
 const c=report.counts;
 box.append(node('p',c.true_accepts+'/'+c.supported_cases+' supported claims kept; '+c.true_abstentions+'/'+c.unsupported_cases+' unsupported claims rejected.'));
 box.append(node('p','False acceptances: '+c.false_accepts+' · supported claims rejected: '+c.false_abstentions+'.'));
 box.append(node('p',report.annotation));
 const details=node('details','');details.append(node('summary','Inspect the '+report.outcomes.length+' held-out decisions'));
 for(const [index,out] of report.outcomes.entries()){
  const test=report.cases[index],card=node('details','');
  card.append(node('summary',(out.accepted===out.expected_supported?'Matches expectation: ':'Mismatch: ')+test.id));
  card.append(node('p','Candidate ('+test.category+'): '+out.finding.text));
  card.append(node('p','Expected '+(out.expected_supported?'supported':'insufficient')+'; reviewer '+(out.accepted?'supported':'insufficient')+' ('+out.reason+').'));
  card.append(node('p','Annotation: '+test.rationale));
  for(const citation of out.finding.citations){card.append(node('blockquote',citation.quote));card.append(sourceLink({id:citation.id,source:'arxiv',pdf_url:'/api/pdf?id='+encodeURIComponent(citation.id)},citation.id+' · abstract evidence · open saved PDF'))}
  details.append(card);
 }
 box.append(details);
 box.append(node('p','First measured '+new Date(report.completed_at).toLocaleString()+' · '+report.model_calls+' local model call. Opening this report makes no model call.'));
 for(const limit of report.limits)box.append(node('p',limit));
}
function renderGeneratedQuality(box,report){
 box.append(node('h3','Historical generated-output audit'));
 if(!report||report.status!=='complete'){
  box.append(node('p',report?.status==='stale'?'The audited source snapshot changed. A fresh source check is required.':'No approved generated-output audit is available.'));return;
 }
 const c=report.counts;
 box.append(node('p',c.authentic_unique_quotes+'/'+c.unique_quotes+' distinct generated quotations match their recorded sources; '+c.invalid_citation_bindings+' invalid source, version, checksum, or physical-page bindings.'));
 box.append(node('p','Manual review of '+c.annotated_claims+' selected claims: '+c.supported_claims+' supported; '+c.unsupported_claims+' unsupported. These counts include earlier failures and do not estimate current-release accuracy.'));
 box.append(node('p','Selected disagreement checks: '+c.correct_abstentions+' correct abstentions; '+c.missed_abstentions+' missed abstention.'));
 box.append(node('p',report.annotation));
 const details=node('details','');details.append(node('summary','Inspect '+c.outputs+' generated outputs and annotations'));
 for(const test of report.cases){
  const card=node('details','');card.append(node('summary',test.id+' · '+test.stage));
  card.append(node('p',test.authentic_unique_quotes+'/'+test.unique_quotes+' distinct quotations verified; '+test.reported_relations+' reported concept-to-concept relations.'));
  const basis=(citation)=>{
   card.append(node('blockquote',citation.quote));
   const source=test.source_snapshots.find(s=>s.id===citation.id);
   let href='/api/pdf?id='+encodeURIComponent(citation.id);
   if(citation.page&&source?.pdf_sha256)href+='&sha256='+encodeURIComponent(source.pdf_sha256)+'#page='+citation.page;
   const a=node('a',citation.id+(citation.page?' · physical page '+citation.page:' · abstract')+' · open saved PDF');a.href=href;a.target='_blank';a.rel='noopener';card.append(a);
  };
  for(const claim of test.claims){card.append(node('h4',claim.id+' · '+claim.verdict));card.append(node('p','Generated claim: '+claim.observed_text));card.append(node('p','Annotation: '+claim.rationale));for(const quote of claim.basis)basis(quote)}
  for(const check of test.abstentions)card.append(node('p',check.category+': expected '+(check.expected_insufficient?'insufficient':'supported')+'; generated '+(check.observed_insufficient?'insufficient':'supported')+'. '+check.rationale));
  if(test.coverage.length){
   const covered=test.coverage.filter(check=>check.covered).length;
   card.append(node('p','Manual answer coverage: '+covered+'/'+test.coverage.length+' selected source-backed conditions covered.'));
   for(const check of test.coverage){card.append(node('p',(check.covered?'Covered: ':'Omitted: ')+check.id+'. '+check.rationale));basis(check.basis)}
  }
  details.append(card);
 }
 box.append(details,node('p','Audited '+new Date(report.audited_at).toLocaleString()+' · '+report.model_calls+' new model calls.'));
 for(const limit of report.limits)box.append(node('p',limit));
}
el('close-quality').onclick=()=>el('quality-dialog').close();
el('open-quality').onclick=async()=>{
 el('quality-dialog').showModal();el('quality-results').replaceChildren(node('p','Loading measured checks…'));
 try{renderQuality(await api('/api/quality'))}catch(error){el('quality-results').replaceChildren(node('p',error.message))}
};
