"""Transit preprocessing: the raw bus records of each snapshot become the network the app computes on.

`source` reads stops and restores runs, `places` groups stops into places, `cycles` reads every route as one round
trip that its patterns run along, `walking` connects stops over the street network, and `build` writes
<region>/build/journey/<snapshot>.json.
"""
