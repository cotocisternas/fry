"""Reproduce illustrative FRY prices; not a capacity or complete bill estimate."""
import json
from pathlib import Path

RATES = {
    'hours': 730, 'traffic_days': 30,
    's3_tiers': [[51200, .0405], [512000, .039], [None, .037]],
    'egress_tiers': [[10240, .150], [51200, .138], [153600, .126], [None, .114]],
    'put': .000007, 'get': .00000056,
    'node_hour': .6426, 'nodes': 6, 'node_disk_gib': 100, 'ebs_gib_month': .152,
    'eks_hour': .1, 'rds_multi_az_hour': 3.04167, 'rds_gib': 3072, 'rds_gib_month': .438,
    'nlb_hour': .034, 'nlb_gb': .00825, 'nat_gateways': 3, 'nat_hour': .093,
    'ipv4_count': 6, 'ipv4_hour': .005,
}

def tiered(volume, tiers):
    total = 0
    previous = 0
    for ceiling, rate in tiers:
        end = volume if ceiling is None else min(volume, ceiling)
        total += max(0, end - previous) * rate
        if ceiling is None or volume <= ceiling:
            break
        previous = ceiling
    return total

def estimate(million_day=20, mean_kb=250, storage_days=30, external_percent=50):
    r=RATES
    n=million_day*1e6
    monthly=n*r['traffic_days']
    size=mean_kb*1000
    stored_bytes=n*size*storage_days
    traffic_bytes=monthly*size
    # Storage: documented binary GB. Networking: explicit conservative decimal-GB model.
    external_gb=traffic_bytes*external_percent/100/1e9
    components={
        'S3 stored messages':tiered(stored_bytes/2**30,r['s3_tiers']),
        'S3 PUT + one GET per message':monthly*(r['put']+r['get']),
        'Internet downloads':tiered(max(0,external_gb-100),r['egress_tiers']),
        'NLB hour + byte dimension':r['hours']*r['nlb_hour']+2*traffic_bytes/1e9*r['nlb_gb'],
        'Illustrative compute + database + fixed network':
            r['hours']*(r['nodes']*r['node_hour']+r['eks_hour']+r['rds_multi_az_hour']+
                        r['nat_gateways']*r['nat_hour']+r['ipv4_count']*r['ipv4_hour'])+
            r['nodes']*r['node_disk_gib']*r['ebs_gib_month']+r['rds_gib']*r['rds_gib_month'],
    }
    return {'million_day':million_day,'mean_kb':mean_kb,'storage_days':storage_days,
            'external_percent':external_percent,'stored_decimal_tb':stored_bytes/1e12,
            'stored_tib':stored_bytes/2**40,'external_decimal_tb':external_gb/1000,
            'components':components,'subtotal_usd':sum(components.values())}

if __name__=='__main__':
    assert abs(tiered(51200,RATES['s3_tiers'])-2073.6)<1e-7
    assert abs(tiered(51201,RATES['s3_tiers'])-2073.639)<1e-7
    assert estimate(external_percent=0)['components']['Internet downloads']==0
    assert estimate()['components']['S3 PUT + one GET per message']==4536
    data={'checked':'2026-10-17','region':'sa-east-1','currency':'USD','rates':RATES,
          'scenarios':[estimate(mean_kb=k) for k in (100,250,500)],
          'egress_sensitivity':[estimate(external_percent=p) for p in (0,50,100)]}
    Path(__file__).with_suffix('.json').write_text(json.dumps(data,indent=2)+'\n')
    for row in data['scenarios']:
        print(f"{row['mean_kb']}KB: ${row['subtotal_usd']:,.2f}; {row['components']}")
