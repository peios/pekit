"""Builds a greeting. A library that depends on words."""

from words import GREETING


def greet(name):
    return f"{GREETING}, {name}"
