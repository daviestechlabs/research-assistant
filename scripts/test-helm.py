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
