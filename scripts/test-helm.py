#!/usr/bin/env python3
"""Verify generic operator rendering and rejected installation configurations."""
import argparse
import copy
import json
import subprocess
from pathlib import Path
import jsonschema
import yaml

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--helm', default='helm')
args = parser.parse_args()
subprocess.run(['python3', str(root/'scripts/package-operator.py'), '--check'], check=True)
values = json.loads((root/'operator/samples/desk-values.yaml').read_text())
schema = json.loads((root/'charts/research-desk/values.schema.json').read_text())
jsonschema.validate(values, schema)
for field, change in [('replicaCount', 2), ('auth', {**values['auth'], 'allowedSubjects':[]}), ('storage', {**values['storage'], 'catalogFile':'../escape.sqlite'}), ('image', {**values['image'],'digest':'latest'})]:
    bad = copy.deepcopy(values); bad[field] = change
    try:
        jsonschema.validate(bad, schema)
    except jsonschema.ValidationError:
        continue
    raise AssertionError('Unsafe field accepted: '+field)
def render(chart, sample):
    text = subprocess.check_output([args.helm,'template','research-example',str(root/'charts'/chart),'-n','research','-f',str(root/'operator/samples'/sample)],text=True)
    return [item for item in yaml.safe_load_all(text) if item]
app=render('research-desk','desk-values.yaml')
pod=next(item for item in app if item['kind']=='Deployment')['spec']['template']['spec']
assert not pod['automountServiceAccountToken']
assert pod['volumes'][0]['persistentVolumeClaim']['claimName']==values['storage']['existingClaim']
assert not any(item['kind']=='PersistentVolumeClaim' for item in app)
config=next(item for item in app if item['kind']=='ConfigMap')['data']
assert config['RESEARCH_LIMIT_OWNER']=='litellm'
assert config['RESEARCH_LITELLM_ORIGIN']=='http://litellm.models.svc.cluster.local:4000'
operator=render('research-desk-operator','operator-values.yaml')
assert not any(item['kind'] in ('ClusterRole','ClusterRoleBinding','PersistentVolumeClaim') for item in operator)
role=next(item for item in operator if item['kind']=='Role')
assert not any('*' in rule['resources'] or 'persistentvolumeclaims' in rule['resources'] for rule in role['rules'])
assert not any(item['kind']=='CiliumNetworkPolicy' for item in operator)
print('Generic chart render, CRD parity, private auth, namespace RBAC, and external storage checks pass.')

import tempfile
indexed=copy.deepcopy(values)
indexed['indexing']={'enabled':True,'configMapName':'private-indexing','directory':'indexing','credentialEnv':[{'name':'RESEARCH_INDEX_STORAGE_ACCESS','secretName':'storage-reader','key':'access'}],'nodeSelector':{},'resources':{'requests':{'cpu':'250m','memory':'256Mi'},'limits':{'cpu':'2','memory':'2Gi'}},'nasCIDRs':['203.0.113.8/32'],'nasPorts':[443],'trustConfigMapName':'private-ca'}
jsonschema.validate(indexed,schema)
with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as sample:
    json.dump(indexed,sample);sample.flush()
    output=subprocess.check_output([args.helm,'template','research-example',str(root/'charts/research-desk'),'-n','research','-f',sample.name],text=True)
    resources=[item for item in yaml.safe_load_all(output) if item]
worker=next(item for item in resources if item['kind']=='Deployment' and item['metadata']['name'].endswith('-indexer'))['spec']['template']['spec']
assert worker['automountServiceAccountToken'] is False
container=worker['containers'][0]
assert container['args']==['index-worker']
assert not container.get('envFrom')
assert 'RESEARCH_MODEL_KEY' not in [item['name'] for item in container['env']]
policy=next(item for item in resources if item['kind']=='NetworkPolicy' and item['metadata']['name'].endswith('-indexer'))['spec']
assert policy['ingress']==[]
assert 4000 not in [port['port'] for rule in policy['egress'] for port in rule.get('ports',[])]
print('Index worker rendering, private configuration mounts, credential boundary, and restricted network checks pass.')

worker_digest = 'sha256:' + '1' * 64
worker_values = copy.deepcopy(values)
worker_values['indexing'] = {**json.loads((root/'charts/research-desk/values.yaml').read_text())['indexing'], **worker_values.get('indexing', {}), 'enabled':True, 'configMapName':'collections', 'imageDigest':worker_digest}
jsonschema.validate(worker_values, schema)
rendered = subprocess.check_output([args.helm, 'template', 'research-desk', str(root/'charts/research-desk'), '-n', 'research', '-f', '-'], input=json.dumps(worker_values), text=True)
worker = next(r for r in yaml.safe_load_all(rendered) if r and r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
assert worker['spec']['template']['spec']['containers'][0]['image']==values['image']['repository']+'@'+worker_digest
print('Independent immutable worker image rendering passes.')

sync_values = copy.deepcopy(worker_values)
sync_values['arxivSync'] = {**json.loads((root/'charts/research-desk/values.yaml').read_text())['arxivSync'], 'enabled':True, 'imageDigest':'sha256:'+'2'*64, 'fromDate':'2026-01-01'}
jsonschema.validate(sync_values, schema)
text = subprocess.check_output([args.helm,'template','research-desk',str(root/'charts/research-desk'),'-n','research','-f','-'],input=json.dumps(sync_values),text=True)
sync_resources = [r for r in yaml.safe_load_all(text) if r]
cron = next(r for r in sync_resources if r['kind']=='CronJob')
assert cron['spec']['concurrencyPolicy']=='Forbid'
sync_pod = cron['spec']['jobTemplate']['spec']['template']['spec']
assert not sync_pod['automountServiceAccountToken']
assert not sync_pod['containers'][0].get('envFrom') and not sync_pod['containers'][0].get('env')
assert sync_pod['containers'][0]['image'].endswith('@'+'sha256:'+'2'*64)
sync_worker = next(r for r in sync_resources if r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
assert sync_worker['spec']['template'] == worker['spec']['template'], 'Enabling downloads restarted the existing indexing worker'
for key, change in [('imageDigest','latest'),('directory','../escape'),('maxDownloadMiB',131073),('fromDate','')]:
    bad = copy.deepcopy(sync_values); bad['arxivSync'][key]=change
    try: jsonschema.validate(bad, schema)
    except jsonschema.ValidationError: continue
    raise AssertionError('Unsafe arXiv sync field accepted: '+key)
print('Scheduled arXiv updates, immutable images, no credentials, bounded storage, and unchanged indexer rendering pass.')

automatic = copy.deepcopy(sync_values)
automatic['indexing']['arxivUpdatesEnabled'] = True
automatic['indexing']['arxivAutoMerge'] = {'enabled':True,'intervalHours':24,'minFreeGiB':64}
jsonschema.validate(automatic,schema)
text = subprocess.check_output([args.helm,'template','research-desk',str(root/'charts/research-desk'),'-n','research','-f','-'],input=json.dumps(automatic),text=True)
objects = [r for r in yaml.safe_load_all(text) if r]
app = next(r for r in objects if r['kind']=='Deployment' and not r['metadata']['name'].endswith('-indexer'))
env = {v['name']:v.get('value') for v in app['spec']['template']['spec']['containers'][0]['env']}
assert env['RESEARCH_ARXIV_AUTO_MERGE']=='true' and env['RESEARCH_ARXIV_AUTO_MERGE_HOURS']=='24'
worker = next(r for r in objects if r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
assert not any(v['name'].startswith('RESEARCH_ARXIV_AUTO_MERGE') for v in worker['spec']['template']['spec']['containers'][0]['env'])
assert not any(v['name']=='RESEARCH_MODEL_KEY' for v in worker['spec']['template']['spec']['containers'][0]['env'])
for section,key,value in [('indexing','arxivUpdatesEnabled',False),('arxivSync','enabled',False)]:
    bad=copy.deepcopy(automatic);bad[section][key]=value
    try:jsonschema.validate(bad,schema)
    except jsonschema.ValidationError:continue
    raise AssertionError('Automatic merge accepted missing prerequisite: '+key)
for key,value in [('intervalHours',5),('intervalHours',169),('minFreeGiB',7)]:
    bad=copy.deepcopy(automatic);bad['indexing']['arxivAutoMerge'][key]=value
    try:jsonschema.validate(bad,schema)
    except jsonschema.ValidationError:continue
    raise AssertionError('Automatic merge accepted unsafe bound: '+key)
print('Automatic merge prerequisites, bounded cadence, storage reserve, and worker credential boundary pass.')

retained = copy.deepcopy(automatic)
retained['indexing']['retention'] = {'enabled':True,'pruneOriginal':True}
jsonschema.validate(retained, schema)
output = subprocess.check_output([args.helm,'template','research-desk',str(root/'charts/research-desk'),'-n','research','-f','-'],input=json.dumps(retained),text=True)
resources = [r for r in yaml.safe_load_all(output) if r]
retained_app = next(r for r in resources if r['kind']=='Deployment' and not r['metadata']['name'].endswith('-indexer'))
env = {e['name']:e.get('value') for e in retained_app['spec']['template']['spec']['containers'][0]['env']}
assert env['RESEARCH_INDEX_RETENTION']=='true' and env['RESEARCH_INDEX_RETENTION_PRUNE_ORIGINAL']=='true'
retained_worker = next(r for r in resources if r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
assert retained_worker['spec']['template']==worker['spec']['template']
bad = copy.deepcopy(retained); bad['indexing']['enabled']=False
try: jsonschema.validate(bad,schema)
except jsonschema.ValidationError: pass
else: raise AssertionError('Retention without indexing accepted')
print('One-backup retention policy, prerequisite, and unchanged worker template checks pass.')

pg = copy.deepcopy(automatic)
pg['postgres'] = {'enabled':True,'workerEnabled':False,'host':'pool.database.svc.cluster.local','database':'research','secretName':'catalog-auth','usernameKey':'username','passwordKey':'password','caSecretName':'catalog-ca','caKey':'ca.crt','namespace':'database','poolerName':'catalog-pool'}
pg['storage']['memoryMiB']=0
jsonschema.validate(pg,schema)
def pg_render(v):
    output=subprocess.check_output([args.helm,'template','research-desk',str(root/'charts/research-desk'),'-n','research','-f','-'],input=json.dumps(v),text=True)
    return [r for r in yaml.safe_load_all(output)if r]
objects=pg_render(pg)
a=next(r for r in objects if r['kind']=='Deployment' and not r['metadata']['name'].endswith('-indexer'))
w=next(r for r in objects if r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
env={e['name']:e.get('value')for e in a['spec']['template']['spec']['containers'][0]['env']}
assert env['RESEARCH_POSTGRES_HOST']==pg['postgres']['host'] and env['RESEARCH_POSTGRES_WRITER']=='false'
assert not any(e['name'].startswith('RESEARCH_POSTGRES_')for e in w['spec']['template']['spec']['containers'][0]['env'])
assert w['spec']['template']==worker['spec']['template'], 'App-only pool adoption changed the pinned worker'
pg['postgres']['workerEnabled']=True
objects=pg_render(pg)
w=next(r for r in objects if r['kind']=='Deployment' and r['metadata']['name'].endswith('-indexer'))
env={e['name']:e.get('value')for e in w['spec']['template']['spec']['containers'][0]['env']}
assert env['RESEARCH_POSTGRES_WRITER']=='true' and 'RESEARCH_MODEL_KEY' not in env
policy=next(r for r in objects if r['kind']=='NetworkPolicy' and r['metadata']['name'].endswith('-indexer'))
assert any(p['port']==5432 for e in policy['spec']['egress']for p in e.get('ports',[]))
for key,value in [('enabled',False),('secretName',''),('caSecretName',''),('host','postgres://user:password@host')]:
    bad=copy.deepcopy(pg);bad['postgres'][key]=value
    try:jsonschema.validate(bad,schema)
    except jsonschema.ValidationError:continue
    raise AssertionError('Unsafe PostgreSQL configuration accepted: '+key)
print('Verified-TLS PostgreSQL references, staged worker adoption, network selection, and rejected credentials-in-URL checks pass.')
