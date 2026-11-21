//go:build !treesitter

package graph

var lexTSFixture = map[string]string{
	"src/util.ts": `export function format(value: string): string {
  return value.trim();
}
export class Store {
  static create(): Store { return new Store(); }
  get(key: string): string { return key; }
}
export const helper = (x: number) => x + 1;
`,
	"src/app.ts": `import { format, Store as S } from './util.js';
import * as util from './util';
import express from 'express';

const app = express();

export function handle(input: string, other: string): string {
  const store = S.create();
  util.helper(1);
  return format(input) + store.get(input);
}

function shadow(format: (s: string) => string) {
  return format('x');
}

app.get('/items/:id', handle);
app.post('/items', (req, res) => res.send(format('y')));

class Base {}
export class Child extends Base implements Thing {
  run() { return handle('z', 'w'); }
}
`,
}

var lexPyFixture = map[string]string{
	"pkg/__init__.py": "",
	"pkg/models.py": `class User:
    def __init__(self, name):
        self.name = name

    @classmethod
    def create(cls, name):
        return cls(name)

def load(name):
    return User.create(name)
`,
	"pkg/api.py": `from fastapi import FastAPI
from .models import User, load
from . import models
import pkg.models as m

app = FastAPI()

@app.get("/users/{name}")
async def get_user(name: str):
    user = load(name)
    models.load(name)
    m.load(name)
    return User.create(name)

def local_shadow(load):
    return load("x")

class Admin(User):
    pass
`,
}

var lexRustFixture = map[string]string{
	"src/lib.rs": `pub mod shapes;
use crate::shapes::{Circle, area};

pub fn total(r: f64) -> f64 {
    let c = Circle::new(r);
    area(&c) + shapes::perimeter(&c)
}
`,
	"src/shapes.rs": `pub struct Circle { pub r: f64 }

impl Circle {
    pub fn new(r: f64) -> Self { Circle { r } }
}

pub trait Shape {
    fn area(&self) -> f64;
}

impl Shape for Circle {
    fn area(&self) -> f64 { 3.14 * self.r * self.r }
}

pub fn area(c: &Circle) -> f64 { c.area() }

pub fn perimeter(c: &Circle) -> f64 { 2.0 * 3.14 * c.r }
`,
}

var lexJavaFixture = map[string]string{
	"src/main/java/com/acme/util/Strings.java": `package com.acme.util;

public final class Strings {
    private Strings() {}

    public static String trim(String s) {
        return s.trim();
    }

    public String instance(String s) {
        return trim(s);
    }
}
`,
	"src/main/java/com/acme/app/Service.java": `package com.acme.app;

import com.acme.util.Strings;
import static com.acme.util.Strings.trim;

public class Service extends Base implements Runnable {
    public void run() {
        Strings.trim(" x ");
        trim(" y ");
        helper();
        Base.make();
    }

    private void helper() {}
}
`,
	"src/main/java/com/acme/app/Base.java": `package com.acme.app;

public class Base {
    public static Base make() { return new Base(); }
}
`,
}
