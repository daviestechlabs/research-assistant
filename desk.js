// Saved sessions and durable jobs share the existing authenticated app routes.
let pendingSemanticConnect=false;
let sessionsEnabled=false,currentSession=null,activeJob=null,sessionDirty=false,sessionSaving=false,sessionLoading=false,saveTimer=null,pollTimer=null;

function sessionData(question){
 return {title:el('session-title').value.trim()||el('query').value.trim()||(source==='dnd'?'D&D research':'Research session'),search:{query:el('query').value.trim()||(source==='dnd'?'*':''),source,limit:source==='dnd'?50:20,broad:el('broad').checked,meaning:el('meaning').checked&&source!=='gutenberg'&&source!=='dnd'},selected_ids:[...selected.keys()],question:question??el('question').value.trim(),task,notes:el('notes').value,include_hypotheses:el('graph-speculative').checked,pdf_mode:el('pdf-mode').checked,search_connect:pendingSemanticConnect};
}
function sessionURL(){
 const url=new URL(location.href);if(currentSession)url.searchParams.set('session',currentSession.id);else url.searchParams.delete('session');history.replaceState(null,'',url.pathname+url.search);
}
function renderComparison(value){
 const labels={agreements:'Agreements',disagreements:'Disagreements',methods:'Methods',limits:'Evidence limits',open_questions:'Open questions'};
 const kinds={reported:'Reported · exact excerpts',inference:'Inference · review the evidence',proposal:'Proposed experiment · speculative'};
 el('answer').replaceChildren();
 for(const section of value.comparison.sections){
  const card=node('section','','comparison-section');card.append(node('h3',labels[section.category]));
  if(section.status==='insufficient'){card.append(node('p','Insufficient evidence. '+section.note));el('answer').append(card);continue}
  if(section.note)card.append(node('p',section.note,'comparison-kind'));
  for(const finding of section.findings){
   card.append(node('p',kinds[finding.kind],'comparison-kind'),node('p',finding.text));
   const details=document.createElement('details');details.append(node('summary','Show cited evidence'));
   for(const citation of finding.citations){
    const quote=node('blockquote',citation.quote),label=citation.id+(citation.page?' · physical page '+citation.page:' · '+citation.scope+' excerpt');
    const doc=value.sources.find(d=>d.id===citation.id);let link;
    if(citation.page){link=node('a',label);link.href=citation.pdf_url;link.target='_blank';link.rel='noopener noreferrer';link.title='Saved version '+citation.version+' · SHA-256 '+citation.sha256}
    else link=sourceLink(doc,label);
    quote.append(node('p',''),link);details.append(quote);
   }
   card.append(details);
  }
  el('answer').append(card);
 }
 el('answer').append(node('p',value.comparison.limits));
}
function markSessionDirty(){
 if(!sessionsEnabled||sessionLoading)return;sessionDirty=true;el('session-status').textContent='Unsaved changes';clearTimeout(saveTimer);
 // Existing sessions save after editing stops. Creating one stays explicit.
 if(currentSession&&!sessionSaving)saveTimer=setTimeout(()=>saveResearchSession().catch(()=>{}),1500);
}
async function saveResearchSession(question){
 if(!sessionsEnabled)throw Error('Session storage is unavailable');
 if(sessionSaving)throw Error('A session save is already in progress');
 clearTimeout(saveTimer);sessionSaving=true;el('save-session').disabled=true;
 const data=sessionData(question),snapshot=JSON.stringify(data);
 try{
  const saved=await api('/api/sessions',{id:currentSession?.id||'',revision:currentSession?.revision||0,data});currentSession=saved;el('session-title').value=saved.data.title;
  // A user can keep writing while the request is in flight.
  sessionDirty=JSON.stringify(sessionData(question))!==snapshot;sessionURL();
  el('session-status').textContent=sessionDirty?'Unsaved changes':'Saved · '+new Date(saved.updated_at).toLocaleTimeString();
  return saved;
 }catch(error){el('session-status').textContent=error.message;throw error}
 finally{sessionSaving=false;el('save-session').disabled=false;if(sessionDirty&&currentSession&&el('session-status').textContent==='Unsaved changes')saveTimer=setTimeout(()=>saveResearchSession().catch(()=>{}),1500)}
}
function showJobResult(job){
 if(!job.result)return;
 if(job.kind==='semantic-search'){revealResults();renderSearchResults(job.result,job.cached);if(pendingSemanticConnect){pendingSemanticConnect=false;chooseTask('hypothesize');selected=new Map(job.result.documents.filter(d=>d.summary_kind!=='missing').slice(0,3).map(d=>[d.id,d]));selectionChanged();saveResearchSession().catch(()=>{})}return}
 if(job.kind==='graph'){
  const value=job.result;clearGraph();graphData={...value,cached:job.cached};graphFocus='';graphZoom=1;
  el('graph-panel').hidden=false;el('content').classList.add('graph-active');el('graph-topic').textContent=value.query;el('graph-limits').textContent=value.limits;
  el('graph-show-speculative').checked=true;el('graph-speculative-display').hidden=!value.edges.some(e=>e.kind==='hypothesis');el('graph-json').disabled=false;el('graph-svg').disabled=false;
  drawGraph();fitGraph();el('graph-detail').replaceChildren(node('p','Select a node or link to inspect its source excerpts.','graph-note'));
 }else if(job.kind==='pdf-answer'){
  clearAnswer();el('answer').hidden=false;
  if(job.result.comparison)renderComparison(job.result);
  for(const statement of job.result.comparison?[]:job.result.statements){
   const p=node('p',({'reported':'Verified page excerpt: ','inference':'Interpretation and inference: ','proposal':'Speculative connection and test: '}[statement.kind]||'')+statement.text);
   for(const citation of statement.citations){const a=node('a',citation.id+' · physical page '+citation.page);a.href=citation.pdf_url;a.target='_blank';a.rel='noopener noreferrer';a.title='Saved version '+citation.version+' · SHA-256 '+citation.sha256;p.append(document.createTextNode(' '),a)}
   el('answer').append(p);
  }
  if(!job.result.comparison)el('answer').append(node('p',job.result.limits));
  el('references').append(node('p',job.cached?'Cached page-cited reading · no new AI call':'Saved page-cited reading · '+job.result.model_calls+' AI call'));
  for(const paper of job.result.papers){const details=document.createElement('details');details.append(node('summary',paper.id+' · saved version '+paper.version));details.append(node('p','Consulted physical pages '+paper.consulted_pages.join(', ')+' of '+paper.physical_pages+'. '+(paper.unreadable_pages?.length?'Pages without usable text: '+paper.unreadable_pages.join(', ')+'.':'No pages lack usable text.')));details.append(node('p','Saved PDF SHA-256: '+paper.sha256));el('references').append(details)}
 }else{
  clearAnswer();el('answer').hidden=false;if(job.result.comparison)renderComparison(job.result);else el('answer').textContent=job.result.answer;
  el('references').append(node('p',job.cached?'Cached verified result · no new AI call':'Saved result · '+(job.result.elapsed_ms/1000).toFixed(1)+' seconds · '+job.result.model_calls+' AI call'));
  for(const d of job.result.sources){const p=document.createElement('p');p.append(sourceLink(d,d.title));el('references').append(p)}
 }
}
function busyJob(job){return job.status==='queued'||job.status==='running'}
function releaseJob(){clearTimeout(pollTimer);activeJob=null;answerBusy=false;graphBusy=false;searchBusy=false;el('job-progress').hidden=true;controls()}
async function pollJob(){
 if(!activeJob)return;const id=activeJob.id;
 try{
  const job=await api('/api/jobs/'+encodeURIComponent(id));if(activeJob?.id!==id)return;activeJob=job;el('job-status').textContent=job.phase;
  if(busyJob(job)){pollTimer=setTimeout(pollJob,document.hidden?6000:2000);return}
  releaseJob();
  if(job.status==='complete')showJobResult(job);else{el('session-status').textContent=job.error||job.phase;if(job.kind==='semantic-search'){el('message').textContent=job.error||job.phase;el('message').className='message error'}else if(job.kind==='graph'){el('graph-panel').hidden=false;el('graph-status').textContent=job.error||job.phase}else{el('answer').hidden=false;el('answer').textContent=job.error||job.phase}}
  await refreshJobHistory();stats();
 }catch(error){el('job-status').textContent=error.message+' · The saved job can be reopened from Sessions.';pollTimer=setTimeout(pollJob,6000)}
}
async function startSavedJob(kind){
 if(answerBusy||graphBusy||searchBusy||activeJob||!selected.size)return;
 if(kind==='answer'&&(el('pdf-mode').checked||[...selected.values()].some(d=>d.source==='dnd')))kind='pdf-answer';
 const basis=kind==='pdf-answer'?'consulted saved PDF pages':'selected abstracts';
 const defaultQuestion=kind==='graph'?(el('query').value.trim()||'Map the selected abstracts'):{explain:'Explain the main ideas in these '+basis+'.',compare:'Compare mechanisms, findings, and evidence limits in these '+basis+'.',hypothesize:'Propose a testable connection from these '+basis+'. Distinguish evidence from speculation.'}[task];
 const question=el('question').value.trim()||defaultQuestion;el('question').value=question;
 kind==='graph'?graphBusy=true:answerBusy=true;controls();
 try{
  const saved=await saveResearchSession(question),job=await api('/api/jobs',{session_id:saved.id,revision:saved.revision,kind});
  if(!busyJob(job)){releaseJob();showJobResult(job);await refreshJobHistory();stats();return}
  activeJob=job;el('job-progress').hidden=false;el('job-status').textContent=job.phase;el('job-progress').scrollIntoView({behavior:'smooth',block:'nearest'});pollJob();
 }catch(error){releaseJob();el('session-status').textContent=error.message}
}
async function refreshJobHistory(){
 if(!currentSession)return;
 const saved=await api('/api/sessions/'+encodeURIComponent(currentSession.id));
 if(saved.revision!==currentSession.revision){el('session-status').textContent='This session changed on another device. Reopen it before editing.';return}
 renderJobHistory(saved.jobs||[]);
}
function renderJobHistory(jobs){
 el('job-list').replaceChildren();
 if(!jobs.length){el('job-list').append(node('p','No research results yet.'));return}
 for(const job of jobs){const b=node('button',({'graph':'Knowledge graph','pdf-answer':'PDF reading','semantic-search':'Meaning search'}[job.kind]||'Research answer')+' · '+job.status+(job.cached?' · cached':'')+' · '+new Date(job.updated_at).toLocaleString(),'quiet-button');b.type='button';
  b.onclick=async()=>{if(activeJob)return;try{const result=await api('/api/jobs/'+job.id);if(busyJob(result)){activeJob=result;setJobBusy(result);el('job-progress').hidden=false;controls();pollJob()}else if(result.status==='complete')showJobResult(result);else el('session-status').textContent=result.error||result.phase}catch(error){el('session-status').textContent=error.message}};
  el('job-list').append(b);
 }
}
async function openResearchSession(id){
 if(activeJob||sessionSaving)throw Error('Wait for the current operation before changing sessions');
 if(sessionDirty&&currentSession)await saveResearchSession();
 clearTimeout(saveTimer);sessionLoading=true;searchVersion++;searchBusy=false;
 try{
  const saved=await api('/api/sessions/'+encodeURIComponent(id));currentSession=saved;
  el('session-title').value=saved.data.title;el('query').value=saved.data.search.query;source=saved.data.search.source||'';el('broad').checked=!!saved.data.search.broad;el('question').value=saved.data.question;el('notes').value=saved.data.notes;el('graph-speculative').checked=saved.data.include_hypotheses;
  el('pdf-mode').checked=!!saved.data.pdf_mode;el('meaning').checked=!!saved.data.search.meaning;pendingSemanticConnect=!!saved.data.search_connect;
  selected=new Map((saved.sources||[]).map(d=>[d.id,d]));chooseTask(saved.data.task);selectionChanged();revealResults();route(false);sessionURL();el('desk').classList.add('opened');el('clear').hidden=!el('query').value;
  el('results').replaceChildren();
  if(saved.data.search.meaning){const job=(saved.jobs||[]).find(j=>j.kind==='semantic-search'&&j.status==='complete');if(job)renderSearchResults((await api('/api/jobs/'+job.id)).result,job.cached);else el('message').textContent='Saved meaning-search query. Run it explicitly to create a result.'}
  else if(saved.data.search.query){const results=await api('/api/search',saved.data.search);for(const d of results.documents)el('results').append(resultCard(d));el('message').textContent='Reopened “'+saved.data.title+'” · saved selections and notes'}
  else el('message').textContent='Reopened “'+saved.data.title+'”';
  renderJobHistory(saved.jobs||[]);sessionDirty=false;el('session-status').textContent='Saved on '+new Date(saved.updated_at).toLocaleString();el('sessions-dialog').close();
  const running=(saved.jobs||[]).find(busyJob);
  if(running){activeJob=running;setJobBusy(running);el('job-progress').hidden=false;controls();pollJob()}
  else{const last=(saved.jobs||[]).find(j=>j.status==='complete');if(last)showJobResult(await api('/api/jobs/'+last.id))}
 }finally{sessionLoading=false;controls()}
}
async function showResearchSessions(){
 el('sessions-dialog').showModal();el('sessions-list').replaceChildren(node('p','Loading saved sessions…'));
 try{const items=await api('/api/sessions');el('sessions-list').replaceChildren();if(!items.length)el('sessions-list').append(node('p','Save a search or start research to create your first session.'));
  for(const item of items){const b=node('button',item.data.title,'quiet-button');b.append(node('small',item.data.selected_ids.length+' selected sources · '+new Date(item.updated_at).toLocaleString()));b.onclick=()=>openResearchSession(item.id).catch(error=>{el('sessions-list').prepend(node('p',error.message))});el('sessions-list').append(b)}
 }catch(error){el('sessions-list').replaceChildren(node('p',error.message))}
}
async function initSavedDesk(){
 try{const capabilities=await api('/api/stats');sessionsEnabled=!!capabilities.sessions_enabled;if(!sessionsEnabled)return;
  el('open-sessions').hidden=false;el('session-tools').hidden=false;el('session-notes').hidden=false;
  const originalSelectionChanged=selectionChanged;selectionChanged=()=>{originalSelectionChanged();markSessionDirty()};
  el('notes').addEventListener('input',markSessionDirty);el('session-title').addEventListener('input',markSessionDirty);el('question').addEventListener('input',markSessionDirty);el('query').addEventListener('input',markSessionDirty);el('graph-speculative').addEventListener('change',markSessionDirty);
  el('meaning').addEventListener('change',()=>{if(el('meaning').checked){el('smart').checked=false;el('rerank').checked=false}markSessionDirty();stats()});
  el('pdf-mode').addEventListener('change',()=>{clearAnswer();controls();markSessionDirty()});
  for(const button of document.querySelectorAll('[data-task]'))button.addEventListener('click',markSessionDirty);
  el('save-session').onclick=()=>saveResearchSession().catch(()=>{});el('open-sessions').onclick=showResearchSessions;el('close-sessions').onclick=()=>el('sessions-dialog').close();
  const startNewSession=async()=>{try{if(activeJob||sessionSaving)throw Error('Wait for the current operation before changing sessions');if(sessionDirty&&currentSession)await saveResearchSession();sessionLoading=true;currentSession=null;home();el('session-title').value='';el('notes').value='';el('question').value='';el('pdf-mode').checked=false;el('meaning').checked=false;controls();el('session-status').textContent='New session · save it when you are ready';sessionDirty=false;sessionLoading=false;el('sessions-dialog').close();el('query').focus()}catch(error){el('session-status').textContent=error.message}};
  el('new-session').onclick=startNewSession;el('home').onclick=startNewSession;
  el('ask').onclick=()=>startSavedJob('answer');el('generate-graph').onclick=()=>startSavedJob('graph');
  el('cancel-job').onclick=async()=>{if(!activeJob)return;try{activeJob=await api('/api/jobs/'+activeJob.id+'/cancel',{});pollJob()}catch(error){el('job-status').textContent=error.message}};
  window.addEventListener('beforeunload',event=>{if(sessionDirty){event.preventDefault();event.returnValue=''}});
  const id=new URL(location.href).searchParams.get('session');if(id)await openResearchSession(id);
 }catch(error){el('session-status').textContent=error.message}
}
initSavedDesk();

function setJobBusy(job){searchBusy=job.kind==='semantic-search';graphBusy=job.kind==='graph';answerBusy=!searchBusy&&!graphBusy}
async function startSemanticSearch({push=true,connect=false}={}){
 if(!sessionsEnabled||activeJob||searchBusy||answerBusy||graphBusy||!el('query').value.trim())return;
 pendingSemanticConnect=connect;if(connect)chooseTask('hypothesize');searchBusy=true;searchVersion++;clearGraph();clearAnswer();revealResults();route(push);controls();el('message').textContent='Saving the meaning-search query…';el('desk').classList.add('opened');
 try{
  const saved=await saveResearchSession(),job=await api('/api/jobs',{session_id:saved.id,revision:saved.revision,kind:'semantic-search'});
  if(!busyJob(job)){releaseJob();showJobResult(job);await refreshJobHistory();stats();return}
  activeJob=job;el('job-progress').hidden=false;el('job-status').textContent=job.phase;pollJob();
 }catch(error){releaseJob();el('message').textContent=error.message;el('message').className='message error'}
}
