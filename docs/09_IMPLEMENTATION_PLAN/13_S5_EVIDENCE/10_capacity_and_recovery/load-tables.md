| run | stage | users cmd+read | cmd ok/s | cmd P50 / P95 / P99 ms | cmd err/timeouts | read ok/s | read P50 / P95 / P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | DB conns max / active mean | lock waiters max (samples), longest | deadlocks / rollbacks | outbox backlog max / relay lag P95 / max s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| run1 | 1 | 1+1 | 30.5 | 23.4 / 68.4 / 77.8 | 0/0 of 1524 | 127.5 | 6.0 / 24.2 / 29.6 | 0/0 of 6373 | 1.78 | 0.63 | 0.45 | 2 / 0.8 | 0 (0/50), 0.000 s | 0 / 0 | 30 / 0.99 / 3.94 | 1.97→3.02 | 48822 |
| run2 | 1 | 1+1 | 29.5 | 23.4 / 67.9 / 96.8 | 0/0 of 1476 | 123.8 | 5.9 / 24.2 / 32.1 | 0/0 of 6188 | 1.74 | 0.74 | 0.43 | 2 / 0.7 | 0 (0/50), 0.000 s | 0 / 0 | 31 / 0.99 / 3.98 | 1.82→6.43 | 48273 |
| run3-two-workers | 1 | 4+4 | 96.5 | 30.2 / 86.5 / 95.1 | 0/0 of 4826 | 398.7 | 7.5 / 31.8 / 36.3 | 0/0 of 19933 | 5.68 | 0.74 | 1.38 | 8 / 3.4 | 1 (1/50), 0.007 s | 0 / 0 | 99 / 0.89 / 3.62 | 4.71→8.49 | 48285 |
| run1 | 2 | 2+2 | 56.3 | 25.8 / 74.4 / 81.3 | 0/0 of 2813 | 233.5 | 6.4 / 27.0 / 31.5 | 0/0 of 11674 | 3.32 | 0.65 | 0.78 | 4 / 1.6 | 1 (1/50), 0.004 s | 0 / 0 | 59 / 1.00 / 3.97 | 2.94→4.96 | 48766 |
| run2 | 2 | 2+2 | 52.1 | 27.1 / 79.2 / 100.9 | 0/0 of 2605 | 214.2 | 6.6 / 28.7 / 37.1 | 0/0 of 10710 | 3.04 | 0.82 | 0.72 | 4 / 1.8 | 0 (0/50), 0.000 s | 0 / 0 | 53 / 1.00 / 4.94 | 6.40→6.82 | 47321 |
| run3-two-workers | 2 | 8+8 | 158.7 | 36.2 / 104.2 / 118.9 | 0/0 of 7935 | 669.1 | 8.8 / 37.5 / 43.8 | 0/0 of 33455 | 9.38 | 0.95 | 2.39 | 16 / 6.2 | 2 (5/50), 0.011 s | 0 / 0 | 359 / 7.47 / 17.90 | 7.89→13.69 | 48055 |
| run1 | 3 | 4+4 | 99.9 | 28.9 / 83.5 / 92.8 | 0/0 of 4994 | 413.9 | 7.3 / 30.4 / 34.9 | 0/0 of 20694 | 5.90 | 0.72 | 1.40 | 8 / 3.8 | 1 (2/50), 0.004 s | 0 / 0 | 112 / 1.02 / 3.99 | 4.72→7.59 | 48626 |
| run2 | 3 | 4+4 | 95.4 | 30.3 / 87.3 / 97.8 | 0/0 of 4772 | 395.3 | 7.5 / 32.1 / 36.4 | 0/0 of 19763 | 5.62 | 0.77 | 1.34 | 8 / 3.5 | 2 (2/50), 0.011 s | 0 / 0 | 96 / 1.01 / 5.46 | 6.43→10.12 | 48363 |
| run3-two-workers | 3 | 16+16 | 183.9 | 64.5 / 180.4 / 203.4 | 0/0 of 9195 | 721.2 | 15.1 / 72.7 / 84.6 | 0/0 of 36059 | 10.88 | 1.65 | 2.80 | 20 / 7.6 | 2 (10/50), 0.039 s | 0 / 0 | 937 / 9.69 / 29.71 | 12.83→18.84 | 47925 |
| run1 | 4 | 8+8 | 160.3 | 35.4 / 102.3 / 117.3 | 0/0 of 8015 | 674.2 | 8.8 / 37.1 / 45.0 | 0/0 of 33712 | 9.44 | 0.96 | 2.45 | 16 / 5.9 | 1 (4/50), 0.013 s | 0 / 0 | 3397 / 20.34 / 22.29 | 7.06→15.84 | 48172 |
| run2 | 4 | 8+8 | 160.4 | 35.7 / 103.6 / 116.5 | 0/0 of 8019 | 669.2 | 8.8 / 37.6 / 43.9 | 0/0 of 33462 | 9.40 | 0.92 | 2.39 | 16 / 6.1 | 1 (3/50), 0.009 s | 0 / 0 | 3533 / 20.85 / 23.36 | 9.63→12.34 | 48080 |
| run1 | 5 | 16+16 | 192.3 | 62.2 / 172.1 / 188.6 | 0/0 of 9615 | 751.1 | 14.5 / 70.0 / 81.2 | 0/0 of 37557 | 11.24 | 1.57 | 2.87 | 20 / 7.6 | 2 (8/50), 0.017 s | 0 / 0 | 8294 / 45.47 / 50.69 | 14.98→20.30 | 48064 |
| run2 | 5 | 16+16 | 188.0 | 63.6 / 176.0 / 192.5 | 0/0 of 9398 | 731.9 | 14.9 / 72.2 / 82.5 | 0/0 of 36594 | 11.18 | 1.56 | 2.87 | 20 / 7.8 | 2 (6/50), 0.047 s | 0 / 0 | 8455 / 47.09 / 52.40 | 12.40→17.42 | 47704 |
| run1 | 6 | 32+32 | 182.8 | 135.6 / 363.3 / 420.0 | 0/0 of 9138 | 672.6 | 29.8 / 165.3 / 200.3 | 0/0 of 33632 | 10.54 | 3.55 | 2.81 | 20 / 8.1 | 4 (10/50), 0.124 s | 0 / 0 | 13350 / 82.39 / 85.96 | 19.00→19.86 | 45034 |
| run2 | 6 | 32+32 | 190.4 | 129.4 / 347.3 / 386.7 | 0/0 of 9519 | 700.5 | 28.9 / 161.0 / 188.0 | 0/0 of 35024 | 11.10 | 3.02 | 2.87 | 20 / 7.8 | 2 (12/50), 0.049 s | 0 / 0 | 13651 / 82.99 / 85.86 | 16.27→22.59 | 47865 |
| run1 | 7 | 64+64 | 167.2 | 285.8 / 833.8 / 1054.4 | 0/0 of 8358 | 601.7 | 60.2 / 368.2 / 569.0 | 0/0 of 30086 | 9.38 | 7.36 | 2.53 | 20 / 9.2 | 15 (14/50), 0.492 s | 0 / 0 | 17531 / 117.04 / 121.01 | 18.91→22.58 | 44178 |
| run2 | 7 | 64+64 | 172.4 | 266.0 / 799.7 / 1173.7 | 0/0 of 8620 | 624.0 | 56.3 / 348.4 / 588.8 | 0/0 of 31200 | 10.24 | 6.99 | 2.60 | 20 / 9.7 | 19 (19/50), 4.812 s | 0 / 0 | 17967 / 113.81 / 117.12 | 20.86→18.94 | 47712 |

- run1: 2026-11-12T13:49:33Z – 2026-11-12T13:57:46Z, tenant load.20261112T134933.4bc8; host at start: 08:49:34 up 1 day,  7:45,  1 user,  load average: 1.97, 4.45, 6.43; MemAvailable 48694 MiB; DB 20 → 385 MB
  - stage 1: top waits ['IO:WALSync×24', 'LWLock:WALWrite×5', 'Client:ClientRead×4']; commits 22854; cache hit 1; 
  - stage 2: top waits ['IO:WALSync×34', 'LWLock:WALWrite×24', 'Client:ClientRead×6']; commits 41695; cache hit 1; 
  - stage 3: top waits ['LWLock:WALWrite×83', 'IO:WALSync×45', 'Client:ClientRead×16', 'Lock:transactionid×2']; commits 73726; cache hit 1; 
  - stage 4: top waits ['LWLock:WALWrite×142', 'IO:WALSync×45', 'Client:ClientRead×35', 'Lock:transactionid×4', 'IO:WALWrite×1']; commits 113621; cache hit 1; 
  - stage 5: top waits ['LWLock:WALWrite×189', 'Client:ClientRead×62', 'IO:WALSync×44', 'Lock:transactionid×6', 'LWLock:LockManager×1']; commits 129127; cache hit 0.9997; 
  - stage 6: top waits ['LWLock:WALWrite×188', 'Client:ClientRead×52', 'IO:WALSync×47', 'Lock:transactionid×11', 'Lock:tuple×7', 'LWLock:BufferContent×1']; commits 119420; cache hit 0.996; 
  - stage 7: top waits ['LWLock:WALWrite×163', 'Lock:tuple×88', 'Client:ClientRead×62', 'IO:WALSync×44', 'Lock:transactionid×17', 'LWLock:LockManager×1']; commits 107090; cache hit 0.9933; 
  - slowest commands at stage 7 (P95 ms): stl credit.reserve 1307, pur purchase.draft.prepare 1009, trd quote.activate 901, trd quote.freeze 883, cnx trade_compliance.evaluate 681, trd order.activate 553
  - slowest reads at stage 7 (P95 ms): GET cst.cost_snapshot 678, GET pur.purchase_draft 490, GET trd.order.check 344, GET pur.demand_allocation 291, GET trd.quote.active 104, GET trd.quote.document 103
- run2: 2026-11-12T14:58:51Z – 2026-11-12T15:09:44Z, tenant load.20261112T145851.8155; host at start: 09:58:51 up 1 day,  8:49,  1 user,  load average: 1.97, 3.06, 3.90; MemAvailable 48602 MiB; DB 398 → 740 MB
  - stage 1: top waits ['IO:WALSync×20', 'LWLock:WALWrite×4', 'Client:ClientRead×2']; commits 22174; cache hit 0.9992; 
  - stage 2: top waits ['LWLock:WALWrite×32', 'IO:WALSync×24', 'Client:ClientRead×8']; commits 38370; cache hit 0.992; 
  - stage 3: top waits ['LWLock:WALWrite×81', 'IO:WALSync×40', 'Client:ClientRead×11', 'Lock:transactionid×3', 'IO:WALWrite×2']; commits 70166; cache hit 0.996; 
  - stage 4: top waits ['LWLock:WALWrite×166', 'IO:WALSync×45', 'Client:ClientRead×34', 'Lock:transactionid×1']; commits 113506; cache hit 0.996; 
  - stage 5: top waits ['LWLock:WALWrite×218', 'Client:ClientRead×45', 'IO:WALSync×42', 'Lock:transactionid×7', 'IO:WALWrite×4']; commits 127953; cache hit 0.9927; 
  - stage 6: top waits ['LWLock:WALWrite×198', 'IO:WALSync×45', 'Client:ClientRead×42', 'Lock:transactionid×16', 'LWLock:BufferContent×1', 'Lock:tuple×1']; commits 123847; cache hit 0.9909; 
  - stage 7: top waits ['LWLock:WALWrite×167', 'Lock:tuple×126', 'Client:ClientRead×49', 'IO:WALSync×38', 'Lock:transactionid×22', 'IO:WALWrite×1']; commits 110341; cache hit 0.9646; 
  - slowest commands at stage 7 (P95 ms): stl credit.reserve 1369, pur purchase.draft.prepare 1097, trd quote.freeze 778, trd quote.activate 723, cnx trade_compliance.decide 569, cnx trade_compliance.evaluate 567
  - slowest reads at stage 7 (P95 ms): GET cst.cost_snapshot 753, GET pur.purchase_draft 531, GET trd.order.check 366, GET pur.demand_allocation 327, GET trd.quote.document 113, GET trd.quote.history 109
- run3-two-workers: 2026-11-12T15:10:01Z – 2026-11-12T15:13:33Z, tenant load.20261112T151001.f11d; host at start: 10:10:01 up 1 day,  8:59,  1 user,  load average: 4.68, 9.32, 7.46; MemAvailable 48500 MiB; DB 740 → 916 MB
  - stage 1: top waits ['LWLock:WALWrite×71', 'IO:WALSync×41', 'Client:ClientRead×18', 'Lock:transactionid×1']; commits 70499; cache hit 0.9974; 
  - stage 2: top waits ['LWLock:WALWrite×150', 'Client:ClientRead×50', 'IO:WALSync×44', 'Lock:transactionid×6', 'LWLock:BufferContent×1']; commits 116035; cache hit 0.9973; 
  - stage 3: top waits ['LWLock:WALWrite×186', 'Client:ClientRead×62', 'IO:WALSync×39', 'Lock:transactionid×8', 'Lock:tuple×1']; commits 128591; cache hit 0.9926; 
  - slowest commands at stage 3 (P95 ms): trd quote.activate 218, trd quote.freeze 203, pur purchase.draft.prepare 174, cnx trade_compliance.evaluate 135, stl credit.reserve 132, trd order.review.decide 115
  - slowest reads at stage 3 (P95 ms): GET cst.cost_snapshot 90, GET trd.order.check 75, GET pur.purchase_draft 72, GET pur.demand_allocation 47, GET trd.order 22, GET trd.quote.active 21
