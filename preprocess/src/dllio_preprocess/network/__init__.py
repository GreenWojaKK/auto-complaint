"""Transit preprocessing: the raw bus records of each revision become the network the app computes on.

`source` reads stops and restores runs, `places` groups stops into places, `round_trip` relates each run to the longest
run of its number, `walking` connects stops over the street network, and `build` writes
<region>/build/journey/<revision>.json.
"""
