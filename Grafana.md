# Stopped Nodes Query
```
SELECT 
    s.name AS "Stream Name",
    n.name AS "Node Name"
FROM el_node_log n
JOIN el_streams s ON n.streamid = s.streamid
WHERE status = 'STOPPED';
```


# Failed Nodes Query
```
SELECT 
    s.name AS "Stream Name",
    n.name AS "Node Name"
FROM el_node_log n
JOIN el_streams s ON n.streamid = s.streamid
WHERE status = 'FAILED';
```